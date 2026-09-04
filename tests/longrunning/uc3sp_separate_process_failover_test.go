//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dblease "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	dboutbox "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	// Aliased: this file already has a local `session` for the MQTT session.
	sessioncfg "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Separate-OS-process exclusive failover
//
// The other failover suites (uc3, e2e_failover) run all "instances" as
// in-process *goruntime.Runtime objects and simulate a
// crash by cooperatively cancelling a context + suppressing the graceful lease
// Release (uc3CrashableLeaseStore). That proves takeover LOGIC but not process
// isolation, real broker-session boundaries, or a genuine SIGKILL.
//
// This test runs TWO real OS processes (nodeprocess_harness_test.go re-execs
// the test binary into the gated TestUC3SeparateProcessFailoverNode child),
// both configured Exclusive against ONE real DynamoDB lease table + ONE real
// broker. The parent:
//
//	1. launches nodes A and B; both compete for the single exclusive lease;
//	2. identifies the elected owner from the FIRST NODE_FULL token AND verifies
//	   it against the authoritative DynamoDB lease row (owner + fencing Version);
//	3. drives real traffic and proves the owner is delivering;
//	4. SIGKILLs the verified owner process — no graceful Release runs, so the
//	   successor must wait out the genuine lease TTL and win by conditional
//	   update (real fencing), not a simulated hand-off;
//	5. asserts the standby advances to ServiceLevelFull (its NODE_FULL token)
//	   with a STRICTLY GREATER fencing Version, within uc3FailoverSLO;
//	6. asserts at-least-once delivery of every message across the failover.
//
//	         DynamoDB lease table (cross-process truth, survives SIGKILL)
//	                     ▲            ▲
//	   ┌── node A ───────┘            └─────── node B ──┐
//	   │ (OS process)                     (OS process)  │
//	   │  Exclusive session, shared sessionID/lease key │
//	   └───────────────┬────────────────┬──────────────┘
//	                   ▼                 ▼
//	              real broker      real SQS (ddblocal/flocilocal/mqttlocal
//	                                        shared by the parent, reached by
//	                                        the children via *_ENDPOINT env)
//
// Real SIGKILL: warm──▓▓▓ owner serving ▓▓▓─╳(kill)────░░░ TTL ░░░──▓▓ successor Full
//               └───────────────────────────┴── measured ≤ uc3FailoverSLO ──┘
// ═══════════════════════════════════════════════════════════════════════════

const (
	uc3spMsgCount = 400
	uc3spPartial  = 80
	uc3spTopic    = "uc3sp/output/data"
	// uc3spFailoverSLO is the separate-process failover ceiling. It is calibrated
	// ABOVE the in-process uc3FailoverSLO (15s, ~5.2s observed) because this path
	// adds real cross-process costs to the measured window: the SIGKILL reap
	// (kill blocks on cmd.Wait + <-done), a fresh-process broker-session teardown,
	// and the 150ms child health-poll granularity. Observed ~5.2s; 25s keeps the
	// gate non-flaky on a loaded shared runner while still failing hard on a
	// regression toward the unbounded default (~336s) profile.
	uc3spFailoverSLO = 25 * time.Second

	// uc3spPublishedSLO is the ceiling for the PUBLISHED lease profile
	// (session.HAConfig: 45s LeaseTTL, 10s renew). A SIGKILLed owner leaves no
	// release behind, so the successor must wait out the whole TTL before its
	// conditional Acquire can win, then poll, connect and satisfy its
	// subscriptions. The ceiling is therefore the TTL plus the acquisition and
	// activation work that follows it — not a tuned observation. Compressed
	// timing (uc3spFailoverSLO above) proves the MECHANISM; this proves the
	// profile an operator actually deploys.
	uc3spPublishedSLO = routing.HALeaseTTL + 45*time.Second

	uc3spNodeEnv        = "GOBRIDGE_UC3SP_NODE"
	uc3spBrokerEnv      = "GOBRIDGE_UC3SP_BROKER"
	uc3spQueueEnv       = "GOBRIDGE_UC3SP_QUEUE"
	uc3spLeaseTableEnv  = "GOBRIDGE_UC3SP_LEASE_TABLE"
	uc3spOutboxTableEnv = "GOBRIDGE_UC3SP_OUTBOX_TABLE"
	uc3spSessionEnv     = "GOBRIDGE_UC3SP_SESSION"
	uc3spTopicEnv       = "GOBRIDGE_UC3SP_TOPIC"
	uc3spInstanceEnv    = "GOBRIDGE_UC3SP_INSTANCE"
	uc3spProfileEnv     = "GOBRIDGE_UC3SP_PROFILE"

	// uc3spPublishedProfile selects session.HAConfig in the child node. Any
	// other value keeps the compressed timing the mechanism proof uses.
	uc3spPublishedProfile = "published"
)

// failoverProfile is the lease timing a separate-process failover run drives.
type failoverProfile struct {
	// name appears in the reported samples so two runs are distinguishable.
	name string
	// env is what the child node reads to pick its session.Config.
	env string
	// slo is the failure-detection to ServiceLevelFull ceiling this profile is
	// gated on.
	slo time.Duration
}

// TestUC3SeparateProcessFailover validates exclusive active/standby failover
// across a REAL SIGKILL of a REAL owner OS process, asserting the successor
// reaches ServiceLevelFull with an advanced fencing version within
// uc3spFailoverSLO and that no message is lost across the transition. It runs
// on COMPRESSED lease timing, which proves the mechanism;
// TestUC3PublishedProfileFailover proves the profile operators deploy.
func TestUC3SeparateProcessFailover(t *testing.T) {
	runSeparateProcessFailover(t, failoverProfile{
		name: "separate-process-warm",
		env:  "compressed",
		slo:  uc3spFailoverSLO,
	})
}

// TestUC3PublishedProfileFailover runs the SAME two-process SIGKILL failover on
// the lease profile the project publishes for high-availability clusters
// (session.HAConfig), rather than on the compressed timing the mechanism proof
// uses. It is the release gate for the failover claim: a compressed-timing pass
// says the takeover logic is correct, and says nothing about whether the
// profile an operator deploys meets its objective.
//
// It asserts the same four facts at the published cadence: the successor
// reaches ServiceLevelFull, its fencing version strictly advances, the whole
// transition lands inside the declared ceiling, and every message is delivered
// across it.
func TestUC3PublishedProfileFailover(t *testing.T) {
	runSeparateProcessFailover(t, failoverProfile{
		name: "separate-process-published",
		env:  uc3spPublishedProfile,
		slo:  uc3spPublishedSLO,
	})
}

func runSeparateProcessFailover(t *testing.T, profile failoverProfile) {
	infra := withFreshInfra(t)

	ddbClient := ddblocal.Client(t)
	leaseTable := ddblocal.UniqueTable("uc3sp-leases")
	leaseBootstrap := dblease.NewStore(ddbClient, dblease.WithTableName(leaseTable))
	require.NoError(t, leaseBootstrap.EnsureTable(t.Context()), "lease table")
	ddblocal.CleanupTable(t, ddbClient, leaseTable)
	leaseReader := dblease.NewStore(ddbClient, dblease.WithTableName(leaseTable))

	outboxTable := ddblocal.UniqueTable("uc3sp-outbox")
	outboxStore := dboutbox.NewStore(ddbClient,
		dboutbox.WithTableName(outboxTable), dboutbox.WithStaleClaimDuration(time.Second))
	require.NoError(t, outboxStore.CreateTable(t.Context()), "outbox table")
	ddblocal.CleanupTable(t, ddbClient, outboxTable)

	queueURL, queueClient := setupSQSQueue(t, "uc3sp-in")
	sessionID := mqttlocal.UniqueClientID("uc3sp-session")
	collector := newMQTTCollector(t, uc3spTopic, "uc3sp-collector")

	nodeEnv := func(instanceID string) map[string]string {
		return map[string]string{
			uc3spNodeEnv:        "1",
			uc3spBrokerEnv:      infra.MQTTBroker,
			uc3spQueueEnv:       queueURL,
			uc3spLeaseTableEnv:  leaseTable,
			uc3spOutboxTableEnv: outboxTable,
			uc3spSessionEnv:     sessionID,
			uc3spTopicEnv:       uc3spTopic,
			uc3spInstanceEnv:    instanceID,
			uc3spProfileEnv:     profile.env,
			"DYNAMODB_ENDPOINT": infra.DDBEndpoint,
			"FLOCI_ENDPOINT":    infra.SQSEndpoint,
		}
	}
	instA := mqttlocal.UniqueClientID("uc3sp-A")
	instB := mqttlocal.UniqueClientID("uc3sp-B")
	nodeA := startNodeProcess(t, "A", "TestUC3SeparateProcessFailoverNode", nodeEnv(instA), "NODE_READY", "NODE_FULL")
	nodeB := startNodeProcess(t, "B", "TestUC3SeparateProcessFailoverNode", nodeEnv(instB), "NODE_READY", "NODE_FULL")

	nodeA.awaitToken(t, "NODE_READY", 90*time.Second)
	nodeB.awaitToken(t, "NODE_READY", 90*time.Second)

	// The initial exclusive owner is whichever node reaches ServiceLevelFull
	// (holds the lease + subscriptions satisfied) first. Verify against the
	// authoritative lease row and capture the starting fencing version.
	ownerNode, ownerTok := awaitFirstToken(t, 90*time.Second, "NODE_FULL", nodeA, nodeB)
	ownerInst := strings.TrimPrefix(ownerTok, "NODE_FULL:")
	standbyNode := nodeB
	if ownerNode == nodeB {
		standbyNode = nodeA
	}
	initialLease := requireLeaseOwner(t, leaseReader, sessionID, ownerInst, 15*time.Second)
	t.Logf("UC3SP: initial owner=%s leaseOwner=%s version=%d", ownerNode.name, initialLease.Owner, initialLease.Version)

	// Split-brain guard: the exclusive lease permits exactly one full owner, so
	// the standby must NOT also have reached full ownership. A lease-exclusivity
	// regression (both nodes ServiceLevelFull) would otherwise pass silently.
	if tok, ok := standbyNode.token("NODE_FULL"); ok {
		t.Fatalf("split brain: standby %s reached full ownership (%s) while %s holds the exclusive lease",
			standbyNode.name, tok, ownerNode.name)
	}

	// Prove the owner process is actually delivering before the kill.
	sendBulkToSQS(t, queueClient, queueURL, uc3spMsgCount, nil)
	lrWaitFor(t, 90*time.Second, fmt.Sprintf("%d messages before kill", uc3spPartial),
		func() bool { return collector.count() >= uc3spPartial })

	// Real SIGKILL of the verified owner: no graceful lease Release runs, so the
	// successor must wait out the genuine TTL and win by conditional update.
	warmDetectedAt := clock.System.Now()
	ownerNode.kill(t)

	// The standby's own DeepHealth-derived NODE_FULL token is the ServiceLevelFull
	// barrier; a generous await bound lets a REGRESSION past the SLO still return
	// a real duration for a clean SLO assertion rather than a bare timeout.
	successorTok := standbyNode.awaitToken(t, "NODE_FULL", 3*profile.slo)
	warmDuration := clock.System.Since(warmDetectedAt)
	successorInst := strings.TrimPrefix(successorTok, "NODE_FULL:")

	successorLease := requireLeaseOwner(t, leaseReader, sessionID, successorInst, 15*time.Second)
	require.NotEqual(t, initialLease.Owner, successorLease.Owner, "successor must be a different owner identity")
	require.Greaterf(t, successorLease.Version, initialLease.Version,
		"fencing version must strictly advance on failover: initial=%d successor=%d",
		initialLease.Version, successorLease.Version)

	reportUC3FailoverSamples(t, profile.name, []time.Duration{warmDuration})
	require.LessOrEqualf(t, warmDuration, profile.slo,
		"%s failover to ServiceLevelFull took %s, exceeding the %s objective "+
			"(regression toward the unbounded default profile?)",
		profile.name, warmDuration, profile.slo)

	// At-least-once across the failover: every message eventually arrives.
	//
	// The wait counts DISTINCT payloads, which is what the assertion below
	// checks. Waiting on the raw delivery count instead would return as soon as
	// uc3spMsgCount deliveries had landed — and a failover produces duplicates,
	// so some of those deliveries are repeats of a payload already seen while
	// another has not arrived yet. That is a correct at-least-once outcome the
	// test would then read as a lost message.
	distinctPayloads := func() int {
		unique := make(map[string]struct{}, uc3spMsgCount)
		for _, m := range collector.getMessages() {
			unique[string(m.Payload())] = struct{}{}
		}
		return len(unique)
	}
	lrWaitFor(t, 150*time.Second, fmt.Sprintf("%d distinct payloads received", uc3spMsgCount),
		func() bool { return distinctPayloads() >= uc3spMsgCount })

	delivered := len(collector.getMessages())
	unique := distinctPayloads()
	t.Logf("%s: %d published, %d unique delivered, %d total deliveries (%d duplicates across the failover)",
		profile.name, uc3spMsgCount, unique, delivered, delivered-unique)
	require.GreaterOrEqualf(t, unique, uc3spMsgCount,
		"expected all %d distinct payloads delivered at-least-once, got %d unique", uc3spMsgCount, unique)
}

// requireLeaseOwner polls the authoritative DynamoDB lease row until the owner
// identity (instanceID prefix of "<instance>#<nonce>") matches wantInstance,
// returning the LeaseInfo (owner + fencing version). Fails hard on timeout.
func requireLeaseOwner(
	t *testing.T, reader ports.LeaseStore, sessionID, wantInstance string, timeout time.Duration,
) persistence.LeaseInfo {
	t.Helper()
	var info persistence.LeaseInfo
	lrWaitFor(t, timeout, "lease owner == "+wantInstance, func() bool {
		cur, err := reader.Current(context.Background(), sessionID)
		if err != nil {
			return false
		}
		instanceID, _, _ := strings.Cut(cur.Owner, "#")
		if instanceID == wantInstance {
			info = cur
			return true
		}
		return false
	})
	return info
}

// TestUC3SeparateProcessFailoverNode is the gated child entrypoint launched as
// a separate OS process by TestUC3SeparateProcessFailover. It builds a single
// Exclusive bridge node against the shared broker + DynamoDB lease/outbox
// tables named in its environment, prints NODE_READY once started, and prints
// NODE_FULL once it has WON the lease and reached ServiceLevelFull. It is a
// no-op under a normal test run (self-skips without the env flag).
func TestUC3SeparateProcessFailoverNode(t *testing.T) {
	if os.Getenv(uc3spNodeEnv) != "1" {
		t.Skip("UC3 separate-process failover node helper")
	}
	broker := requiredCrashEnv(t, uc3spBrokerEnv)
	queueURL := requiredCrashEnv(t, uc3spQueueEnv)
	leaseTable := requiredCrashEnv(t, uc3spLeaseTableEnv)
	outboxTable := requiredCrashEnv(t, uc3spOutboxTableEnv)
	sessionID := requiredCrashEnv(t, uc3spSessionEnv)
	topic := requiredCrashEnv(t, uc3spTopicEnv)
	instanceID := requiredCrashEnv(t, uc3spInstanceEnv)

	lease := dblease.NewStore(ddblocal.Client(t), dblease.WithTableName(leaseTable))
	outbox := dboutbox.NewStore(ddblocal.Client(t),
		dboutbox.WithTableName(outboxTable), dboutbox.WithStaleClaimDuration(time.Second))

	// Distinct MQTT ClientID per node (instanceID is unique) but the SAME
	// logical sessionID as the lease key — the cluster shape uc3 uses.
	session := newMQTTSessionWithBroker(t, broker, instanceID, connectivity.SessionExclusive, 64, 5)
	sender := setupMQTTSender(t, session)
	sessionConfig := lrSessionConfig(sessionID)
	if os.Getenv(uc3spProfileEnv) == uc3spPublishedProfile {
		// The published high-availability preset, with the objective the parent
		// gates on declared on the config so the runtime's own budget preflight
		// has to accept it too. A profile whose declared timing the runtime
		// refuses is not a profile anyone can deploy.
		sessionConfig = sessioncfg.HAConfig(sessionID, true)
		sessionConfig.FailoverSLO = uc3spPublishedSLO
	}

	rt := goruntime.New(
		goruntime.WithInstanceID(instanceID),
		goruntime.WithLeaseStore(lease),
		goruntime.WithOutboxStore(outbox),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	if err := rt.AddRoute(task14CrashRoute(instanceID, sessionID, topic),
		newCrashSQSReceiver(t, queueURL), sender, session, &sessionConfig); err != nil {
		t.Fatalf("node AddRoute: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("node Start: %v", err)
	}
	fmt.Fprintf(os.Stdout, "NODE_READY:%s\n", instanceID)

	// Emit NODE_FULL exactly once, when this node has won the exclusive lease
	// AND reached ServiceLevelFull (the successorFull predicate uc3 uses, applied
	// in-process here). A standby simply never reaches it until the owner dies.
	announced := false
	for {
		if !announced && nodeSessionFullOwner(rt, sessionID) {
			fmt.Fprintf(os.Stdout, "NODE_FULL:%s\n", instanceID)
			announced = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// nodeSessionFullOwner reports whether rt is the active ServiceLevelFull owner
// of sessionID: ready for traffic, full service level, and holding the lease
// for that session. Mirrors uc3's in-process successorFull predicate.
func nodeSessionFullOwner(rt *goruntime.Runtime, sessionID string) bool {
	h := rt.DeepHealth(context.Background())
	if !h.ReadyForTraffic || h.ServiceLevel != ports.ServiceLevelFull {
		return false
	}
	for _, sh := range h.Sessions {
		if sh.SessionID == sessionID {
			return sh.HasLease && sh.ServiceLevel == ports.ServiceLevelFull
		}
	}
	return false
}

//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	dynamodblease "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	dynamodbrollout "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// UC-CR — coordinated cluster rollout across REAL separate OS processes, real
// DynamoDB, and the real config codec (design §10 / Phase 5). Each node is a
// bridge.Supervisor in its own process (rollout_node_child_test.go), coordinating
// only through the shared DynamoDB config source + rollout store + lease store.
// Barriers are stdout tokens and store rows — never sleeps.

// rolloutTestbed is the shared coordination substrate for a coordinated cohort:
// the DynamoDB config source (candidate delivery), rollout store (barrier +
// committed artifact), and coordinator lease, all on unique tables the parent can
// read directly and the children share by table name.
type rolloutTestbed struct {
	infra        *testInfra
	bridgeID     string
	members      []string
	rolloutTable string
	leaseTable   string
	configTable  string
	rolloutStore *dynamodbrollout.Store
	loader       *ddbconfig.Loader
	ttl          string // optional GOBRIDGE_ROLLOUT_TTL for the children (e.g. "4s")
	version      int    // the document version last written (base = 1)
}

// newRolloutTestbed provisions the three shared tables, seeds the config source
// with the base config, and returns the testbed. Members boot on the seeded
// config (version 1 after the first Save).
func newRolloutTestbed(t *testing.T, members []string, baseAddress string) *rolloutTestbed {
	t.Helper()
	infra := withFreshInfra(t)
	client := ddblocal.Client(t)
	ctx := context.Background()
	bridgeID := "uccr-bridge"

	rolloutTable := ddblocal.UniqueTable("uccr-rollout")
	rolloutStore := dynamodbrollout.NewStore(client, dynamodbrollout.WithTableName(rolloutTable))
	require.NoError(t, rolloutStore.EnsureTable(ctx))
	ddblocal.CleanupTable(t, client, rolloutTable)

	leaseTable := ddblocal.UniqueTable("uccr-lease")
	leaseStore := dynamodblease.NewStore(client, dynamodblease.WithTableName(leaseTable))
	require.NoError(t, leaseStore.EnsureTable(ctx))
	ddblocal.CleanupTable(t, client, leaseTable)

	configTable := ddblocal.UniqueTable("uccr-config")
	loader := ddbconfig.NewLoader(client,
		ddbconfig.WithTableName(configTable),
		ddbconfig.WithBridgeID(bridgeID),
		ddbconfig.WithRegistry(rolloutRegistry()),
	)
	require.NoError(t, loader.EnsureTable(ctx))
	ddblocal.CleanupTable(t, client, configTable)

	// Seed the base config as document version 1. (The DynamoDB item version is
	// managed separately by Save's CAS; the DOCUMENT version is the one the barrier
	// and members track, so we set it explicitly per generation.)
	require.NoError(t, loader.Save(ctx, rolloutNodeConfig(bridgeID, 1, baseAddress, members)))

	return &rolloutTestbed{
		infra: infra, bridgeID: bridgeID, members: members,
		rolloutTable: rolloutTable, leaseTable: leaseTable, configTable: configTable,
		rolloutStore: rolloutStore, loader: loader, version: 1,
	}
}

// childEnv builds the env for a member child process.
func (b *rolloutTestbed) childEnv(member string) map[string]string {
	membersCSV := ""
	for i, m := range b.members {
		if i > 0 {
			membersCSV += ","
		}
		membersCSV += m
	}
	env := map[string]string{
		rolloutNodeEnv:        "1",
		rolloutNodeMemberEnv:  member,
		rolloutNodeMembersEnv: membersCSV,
		rolloutNodeBridgeEnv:  b.bridgeID,
		rolloutNodeRolloutTbl: b.rolloutTable,
		rolloutNodeLeaseTbl:   b.leaseTable,
		rolloutNodeConfigTbl:  b.configTable,
		"DYNAMODB_ENDPOINT":   b.infra.DDBEndpoint,
	}
	if b.ttl != "" {
		env[rolloutNodeTTLEnv] = b.ttl
	}
	return env
}

// saveChange writes a new config document (a live-safe address delta) to the
// config source and returns the version members will converge to.
func (b *rolloutTestbed) saveChange(t *testing.T, address string) int {
	t.Helper()
	b.version++
	require.NoError(t, b.loader.Save(context.Background(), rolloutNodeConfig(b.bridgeID, b.version, address, b.members)))
	return b.version
}

// currentRollout reads the rollout store row (parent-side authority).
func (b *rolloutTestbed) currentRollout(t *testing.T) persistence.Rollout {
	t.Helper()
	r, err := b.rolloutStore.Current(context.Background())
	require.NoError(t, err)
	return r
}

// TestUCCR1_HappyPathCommitsAcrossProcesses is the N=3 happy path: three real
// bridge processes, sharing only DynamoDB, all Ack a live-safe change and the
// lease-elected coordinator commits it — every member applies the new generation.
// This also measures the staging duration (Q2 sizing input).
func TestUCCR1_HappyPathCommitsAcrossProcesses(t *testing.T) {
	members := []string{
		ddblocal.UniqueTable("mA"), ddblocal.UniqueTable("mB"), ddblocal.UniqueTable("mC"),
	}
	tb := newRolloutTestbed(t, members, "addr/base")

	nodeA := startNodeProcess(t, "A", "TestRolloutNode", tb.childEnv(members[0]),
		rolloutTokReady, rolloutTokCommitted, rolloutTokAborted)
	nodeB := startNodeProcess(t, "B", "TestRolloutNode", tb.childEnv(members[1]),
		rolloutTokReady, rolloutTokCommitted, rolloutTokAborted)
	nodeC := startNodeProcess(t, "C", "TestRolloutNode", tb.childEnv(members[2]),
		rolloutTokReady, rolloutTokCommitted, rolloutTokAborted)
	nodes := []*nodeProcess{nodeA, nodeB, nodeC}

	for _, n := range nodes {
		n.awaitToken(t, rolloutTokReady, 90*time.Second)
	}

	// Drive a live-safe change; every member must observe it, ack, and apply the
	// committed generation.
	start := time.Now()
	version := tb.saveChange(t, "addr/rolled")

	for _, n := range nodes {
		tok := n.awaitToken(t, rolloutTokCommitted, 90*time.Second)
		// token is ROLLOUT_COMMITTED:<member>:<version>
		require.Contains(t, tok, fmt.Sprintf(":%d", version),
			"member %s committed the wrong version (want %d): %q", n.name, version, tok)
	}
	staging := time.Since(start)

	// Parent-side authority: the rollout store row is Committed at this version.
	r := tb.currentRollout(t)
	require.Equal(t, persistence.RolloutCommitted, r.State(), "rollout row must be committed")
	require.Equal(t, version, r.ConfigVersion())
	require.Len(t, r.Acks(), 3, "all three members must have acked")

	t.Logf("UC-CR1 Q2 sizing: N=3 staging duration (propose->all-committed) = %s "+
		"(defaultRolloutTTL=5m has ample margin)", staging.Round(time.Millisecond))
}

// tokenVersion extracts the trailing :<version> integer from a token like
// "ROLLOUT_READY:member:2".
func tokenVersion(t *testing.T, tok string) int {
	t.Helper()
	var v int
	// token is <PREFIX>:<member>:<version>
	parts := tok
	last := -1
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == ':' {
			last = i
			break
		}
	}
	require.GreaterOrEqual(t, last, 0, "malformed token %q", tok)
	_, err := fmt.Sscanf(parts[last+1:], "%d", &v)
	require.NoError(t, err, "token %q has no trailing version", tok)
	return v
}

// TestUCCR3_KilledMemberRejoinsOnCommittedGen is the multi-process end-to-end
// proof of the Phase-5A residual fix (design UC-CR3 / seq 3): after a real SIGKILL
// leaves a rollout unable to complete, the coordinator aborts it on the deadline,
// and the killed member — restarted as a fresh process whose config source still
// holds the REJECTED candidate — rejoins on the last COMMITTED generation, not the
// aborted one. It exercises the durable committed artifact written by parser
// codec into DynamoDB and read back by the restarted process (the round-trip a
// unit test cannot cover).
func TestUCCR3_KilledMemberRejoinsOnCommittedGen(t *testing.T) {
	members := []string{
		ddblocal.UniqueTable("mA"), ddblocal.UniqueTable("mB"), ddblocal.UniqueTable("mC"),
	}
	tb := newRolloutTestbed(t, members, "addr/base")
	tb.ttl = "5s" // short deadline so the missing-ack rollout aborts quickly

	nodeA := startNodeProcess(t, "A", "TestRolloutNode", tb.childEnv(members[0]),
		rolloutTokReady, rolloutTokCommitted, rolloutTokAborted)
	nodeB := startNodeProcess(t, "B", "TestRolloutNode", tb.childEnv(members[1]),
		rolloutTokReady, rolloutTokCommitted, rolloutTokAborted)
	nodeC := startNodeProcess(t, "C", "TestRolloutNode", tb.childEnv(members[2]),
		rolloutTokReady, rolloutTokCommitted, rolloutTokAborted)

	nodeA.awaitToken(t, rolloutTokReady, 90*time.Second)
	nodeB.awaitToken(t, rolloutTokReady, 90*time.Second)
	nodeC.awaitToken(t, rolloutTokReady, 90*time.Second)

	// 1. Commit a real generation so the durable committed artifact exists.
	committedVersion := tb.saveChange(t, "addr/committed")
	nodeA.awaitToken(t, rolloutTokCommitted, 90*time.Second)
	nodeB.awaitToken(t, rolloutTokCommitted, 90*time.Second)
	nodeC.awaitToken(t, rolloutTokCommitted, 90*time.Second)

	// 2. SIGKILL member C, THEN write a new candidate. C is frozen in the roster,
	//    so the rollout can never gather C's ack and the coordinator aborts it on
	//    the deadline. (Killing before the write makes the abort deterministic — no
	//    race on whether C acked.)
	nodeC.kill(t)
	abortedVersion := tb.saveChange(t, "addr/aborted")
	require.Greater(t, abortedVersion, committedVersion)

	// 3. The surviving members observe the abort.
	nodeA.awaitToken(t, rolloutTokAborted, 90*time.Second)
	nodeB.awaitToken(t, rolloutTokAborted, 90*time.Second)

	// 4. Restart C as a fresh process. Its config source now holds the REJECTED
	//    candidate (addr/aborted, version 3), but it must boot on the COMMITTED
	//    generation instead.
	nodeCRestart := startNodeProcess(t, "C2", "TestRolloutNode", tb.childEnv(members[2]),
		rolloutTokReady, rolloutTokCommitted, rolloutTokAborted)
	readyTok := nodeCRestart.awaitToken(t, rolloutTokReady, 90*time.Second)

	got := tokenVersion(t, readyTok)
	require.Equal(t, committedVersion, got,
		"the restarted member booted on version %d, want the committed version %d (not the aborted candidate %d)",
		got, committedVersion, abortedVersion)
}

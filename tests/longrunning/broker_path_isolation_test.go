//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dblease "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	dboutbox "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/netfault"
)

const (
	brokerPathMsgCount = 400
	brokerPathTopic    = "brokerpath/output/data"
	// brokerPathStepDown is the configured non-converged threshold. It is well
	// above a healthy reconnect+reconcile on loopback and well below the lease
	// TTL, so a step-down here can only be the broker-path decision firing.
	brokerPathStepDown = 5 * time.Second
	// brokerPathLeaseTTL is deliberately SIX TIMES the threshold. It is what
	// makes this test a proof rather than an observation: a successor that
	// reaches ServiceLevelFull inside brokerPathFailoverSLO cannot have waited
	// out a natural expiry, so the isolated owner must have RELEASED the lease.
	brokerPathLeaseTTL = 30 * time.Second
	// brokerPathFailoverSLO is the measured ceiling from the broker-path cut to
	// a verified successor at ServiceLevelFull. Expected ~13s: threshold 5s +
	// one renew round + bounded close + settlement grace + release + one
	// acquire poll + reconnect. The ceiling sits under the 30s TTL on purpose.
	brokerPathFailoverSLO = 25 * time.Second
)

// TestBrokerPathIsolation_OwnerStepsDownSoAHealthyStandbyTakesOver proves the
// failure mode the lease machinery cannot see: ONE member loses its path to the
// broker while the lease store stays reachable for everyone. Renewals keep
// succeeding, so nothing expires, no fencing signal fires, and without a
// broker-path decision the isolated owner holds the partition forever while a
// healthy standby waits.
//
// Every other broker test in this suite takes the broker down for ALL members,
// which proves the opposite thing — that a globally unreachable broker is
// survived rather than failed over. Here each member reaches the ONE broker
// through a fault-injection proxy of its own, so a cut isolates exactly one
// member's path.
func TestBrokerPathIsolation_OwnerStepsDownSoAHealthyStandbyTakesOver(t *testing.T) {
	_ = withFreshInfra(t)
	sqsInURL, sqsInClient := setupSQSQueue(t, "brokerpath-in")
	ddbClient := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("brokerpath-leases")
	leaseBootstrap := dblease.NewStore(ddbClient, dblease.WithTableName(leaseTable))
	require.NoError(t, leaseBootstrap.EnsureTable(t.Context()), "lease table")
	ddblocal.CleanupTable(t, ddbClient, leaseTable)
	leaseReader := dblease.NewStore(ddbClient, dblease.WithTableName(leaseTable))

	outboxTable := ddblocal.UniqueTable("brokerpath-outbox")
	outboxStore := dboutbox.NewStore(ddbClient, dboutbox.WithTableName(outboxTable),
		dboutbox.WithStaleClaimDuration(10*time.Second))
	require.NoError(t, outboxStore.CreateTable(t.Context()), "outbox table")
	ddblocal.CleanupTable(t, ddbClient, outboxTable)

	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("brokerpath-session")
	// The collector reaches the broker DIRECTLY: it must keep observing while a
	// member's proxied path is cut, or "no deliveries" would prove nothing.
	collector := newMQTTCollector(t, brokerPathTopic, "brokerpath-collector")
	brokerTarget := strings.TrimPrefix(mqttlocal.BrokerURL(t), "tcp://")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	routeCfg := goruntime.RouteConfig{
		ID:       "brokerpath-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "brokerpath-bind", Address: brokerPathTopic}),
		Bindings: []routing.DestinationBinding{{ID: "brokerpath-bind", SessionID: sessionID}},
	}

	type member struct {
		label      string
		instanceID string
		rt         *goruntime.Runtime
		proxy      *netfault.Proxy
		metrics    *ports.RecordingExporter
		cancel     context.CancelFunc
	}
	var members []*member
	var stopOnce sync.Map

	mkMember := func(label string) *member {
		// One proxy per member: cutting it isolates THIS member's broker path
		// and nothing else.
		proxy := netfault.Start(t, brokerTarget)
		sess := newMQTTSessionWithBroker(t, proxy.URL("tcp"),
			mqttlocal.UniqueClientID(fmt.Sprintf("brokerpath-%s", label)),
			connectivity.SessionExclusive, 65534, 5)
		sender := setupMQTTSender(t, sess)
		sqsRx := newSQSReceiver(t, sqsInURL)

		sc := session.DefaultConfig(sessionID, true)
		sc.LeaseTTL = brokerPathLeaseTTL
		sc.RenewInterval = 2 * time.Second
		sc.RenewJitter = 200 * time.Millisecond
		sc.RenewCallTimeout = time.Second
		sc.StepDownGrace = time.Second
		sc.BrokerHealthStepDown = brokerPathStepDown
		sc.DrainStrategy = persistence.NewFixedPoll(200 * time.Millisecond)
		sc.DrainBatchSize = 100
		require.NoError(t, sc.Validate(), "session config %s", label)

		rec := &ports.RecordingExporter{}
		instanceID := fmt.Sprintf("brokerpath-%s", label)
		rt := goruntime.New(
			goruntime.WithInstanceID(instanceID),
			goruntime.WithLeaseStore(dblease.NewStore(ddbClient, dblease.WithTableName(leaseTable))),
			goruntime.WithOutboxStore(outboxStore),
			goruntime.WithDLQStore(dlq),
			goruntime.WithMetrics(rec),
		)
		require.NoError(t, rt.AddRoute(routeCfg, sqsRx, sender, sess, &sc), "AddRoute %s", label)
		memberCtx, memberCancel := context.WithCancel(ctx)
		require.NoError(t, rt.Start(memberCtx), "Start %s", label)

		m := &member{label: label, instanceID: instanceID, rt: rt, proxy: proxy, metrics: rec, cancel: memberCancel}
		members = append(members, m)
		return m
	}
	stop := func(m *member) {
		if _, loaded := stopOnce.LoadOrStore(m.label, true); loaded {
			return
		}
		m.cancel()
		_ = m.rt.Stop(context.Background())
	}
	t.Cleanup(func() {
		for _, m := range members {
			stop(m)
		}
	})

	memberForOwner := func(owner string) *member {
		instanceID, _, _ := strings.Cut(owner, "#")
		for _, m := range members {
			if m.instanceID == instanceID {
				return m
			}
		}
		return nil
	}
	atFullWithLease := func(m *member) bool {
		if m == nil {
			return false
		}
		health := m.rt.DeepHealth(context.Background())
		if !health.ReadyForTraffic || health.ServiceLevel != ports.ServiceLevelFull {
			return false
		}
		for _, sh := range health.Sessions {
			if sh.SessionID == sessionID {
				return sh.HasLease && sh.ServiceLevel == ports.ServiceLevelFull
			}
		}
		return false
	}
	currentLease := func() (persistence.LeaseInfo, error) {
		return leaseReader.Current(context.Background(), sessionID)
	}

	mkMember("A")
	mkMember("B")
	gobridgesync(t, 15*time.Second, members[0].rt, members[1].rt)

	var initial persistence.LeaseInfo
	var owner *member
	lrWaitFor(t, 30*time.Second, "initial verified lease owner at ServiceLevelFull", func() bool {
		info, err := currentLease()
		if err != nil {
			return false
		}
		candidate := memberForOwner(info.Owner)
		if !atFullWithLease(candidate) {
			return false
		}
		initial, owner = info, candidate
		return true
	})
	var standby *member
	for _, m := range members {
		if m != owner {
			standby = m
		}
	}
	require.NotNil(t, standby, "a standby member must exist")

	sendBulkToSQS(t, sqsInClient, sqsInURL, brokerPathMsgCount, nil)
	lrWaitForProgress(t, "deliveries before the broker-path cut",
		collector.count, brokerPathMsgCount/4, 30*time.Second, 90*time.Second)

	// Isolate the OWNER's broker path only. The lease store is untouched, and
	// the standby's own proxy keeps forwarding.
	cutAt := clock.System.Now()
	owner.proxy.Cut()

	// The premise of this failure mode, asserted rather than assumed: the
	// isolated owner keeps RENEWING successfully, so its lease expiry keeps
	// advancing and no lease/fencing signal will ever hand the partition over.
	lrWaitFor(t, 3*brokerPathStepDown, "isolated owner keeps renewing while its broker path is down", func() bool {
		info, err := currentLease()
		return err == nil && info.Owner == initial.Owner && info.ExpiresAt.After(initial.ExpiresAt)
	})

	var successor persistence.LeaseInfo
	var successorMember *member
	var failoverDuration time.Duration
	lrWaitFor(t, brokerPathFailoverSLO, "healthy standby owns the lease at ServiceLevelFull", func() bool {
		info, err := currentLease()
		if err != nil || info.Owner == initial.Owner || info.Version <= initial.Version {
			return false
		}
		candidate := memberForOwner(info.Owner)
		if !atFullWithLease(candidate) {
			return false
		}
		successor, successorMember = info, candidate
		failoverDuration = clock.System.Since(cutAt)
		return true
	})
	t.Logf("broker-path failover: cut->successor ServiceLevelFull = %s (ceiling %s, lease TTL %s)",
		failoverDuration.Round(time.Millisecond), brokerPathFailoverSLO, brokerPathLeaseTTL)

	require.Equal(t, standby, successorMember, "the HEALTHY standby must take over, not the isolated owner")
	// Exactly one takeover. The isolated owner re-enters no acquire loop after
	// its step-down, so it cannot win back the row it just released — a
	// re-seizure would show up here as a second fencing increment.
	require.Equal(t, initial.Version+1, successor.Version,
		"exactly one takeover must advance the fencing version; a second increment means the isolated owner re-seized the lease it released")
	require.LessOrEqualf(t, failoverDuration, brokerPathFailoverSLO,
		"broker-path failover took %s, exceeding the %s objective", failoverDuration, brokerPathFailoverSLO)
	// The successor took over well inside the 30s TTL, so the isolated owner
	// released the lease rather than letting it expire.
	require.Less(t, failoverDuration, brokerPathLeaseTTL,
		"takeover inside the lease TTL is what proves a voluntary release rather than an expiry")

	assert.NotEmpty(t, owner.metrics.FindEntries(shared.MetricBrokerHealthStepDown),
		"the isolated owner must emit BrokerHealthStepDown so an operator can alert on it")

	// Conservation: every message still arrives exactly once, through the
	// successor's healthy broker path.
	lrWaitFor(t, 120*time.Second, "every message delivered after the broker-path failover",
		func() bool { return countUnique(collector) >= brokerPathMsgCount })
	msgs := collector.getMessages()
	unique := make(map[string]struct{}, len(msgs))
	for _, message := range msgs {
		unique[string(message.Payload())] = struct{}{}
	}
	assert.GreaterOrEqual(t, len(unique), brokerPathMsgCount, "no message may be lost across a broker-path failover")
	assert.Equal(t, 0, dlq.count(), "DLQ must be empty")
}

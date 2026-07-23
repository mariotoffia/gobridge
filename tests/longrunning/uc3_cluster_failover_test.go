//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

const (
	uc3MsgCount = 2000
	uc3Topic    = "uc3/output/data"
	// uc3FailoverSLO is the asserted failure-detection-to-ServiceLevelFull ceiling
	// for this test's compressed lease profile (LeaseTTL=5s). Observed ~5.1s warm /
	// ~5.2s cold; the ~3x headroom keeps the gate non-flaky while still failing hard
	// on a regression toward the unbounded default profile (~336s). See TEST-4.
	uc3FailoverSLO = 15 * time.Second
)

type uc3CrashableLeaseStore struct {
	ports.LeaseStore
	suppressRelease atomic.Bool
}

func (s *uc3CrashableLeaseStore) Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error {
	if s.suppressRelease.Load() {
		return nil
	}
	return s.LeaseStore.Release(ctx, leaseID, token)
}

func TestUC3ClusterFailover(t *testing.T) {
	_ = withFreshInfra(t)
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc3-in")
	ddbClient := ddblocal.Client(t)
	leaseTable := ddblocal.UniqueTable("uc3-leases")
	leaseBootstrap := dblease.NewStore(ddbClient, dblease.WithTableName(leaseTable))
	require.NoError(t, leaseBootstrap.EnsureTable(t.Context()), "lease table")
	ddblocal.CleanupTable(t, ddbClient, leaseTable)
	newLeaseStore := func() ports.LeaseStore { return dblease.NewStore(ddbClient, dblease.WithTableName(leaseTable)) }
	leaseReader := newLeaseStore()

	outboxTable := ddblocal.UniqueTable("uc3-outbox")
	outboxStore := dboutbox.NewStore(ddbClient, dboutbox.WithTableName(outboxTable), dboutbox.WithStaleClaimDuration(time.Second))
	require.NoError(t, outboxStore.CreateTable(t.Context()), "outbox table")
	ddblocal.CleanupTable(t, ddbClient, outboxTable)

	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc3-session")
	collector := newMQTTCollector(t, uc3Topic, "uc3-collector")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	routeCfg := goruntime.RouteConfig{
		ID:       "uc3-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "uc3-bind", Address: uc3Topic}),
		Bindings: []routing.DestinationBinding{{ID: "uc3-bind", SessionID: sessionID}},
	}

	type instance struct {
		label      string
		instanceID string
		rt         *goruntime.Runtime
		cancel     context.CancelFunc
		lease      *uc3CrashableLeaseStore
	}
	var instances []*instance
	mkInstance := func(label string) *instance {
		mqttSessID := mqttlocal.UniqueClientID(fmt.Sprintf("uc3-%s", label))
		sess := newMQTTSession(t, mqttSessID, connectivity.SessionExclusive)
		mqttSnd := setupMQTTSender(t, sess)
		sqsRx := newSQSReceiver(t, sqsInURL)
		sc := lrSessionConfig(sessionID)
		instanceID := fmt.Sprintf("uc3-%s", label)
		lease := &uc3CrashableLeaseStore{LeaseStore: newLeaseStore()}
		rt := goruntime.New(goruntime.WithInstanceID(instanceID), goruntime.WithLeaseStore(lease), goruntime.WithOutboxStore(outboxStore), goruntime.WithDLQStore(dlq))
		require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, &sc), "AddRoute %s", label)
		instCtx, instCancel := context.WithCancel(ctx)
		require.NoError(t, rt.Start(instCtx), "Start %s", label)
		inst := &instance{label: label, instanceID: instanceID, rt: rt, cancel: instCancel, lease: lease}
		instances = append(instances, inst)
		return inst
	}

	var stopped sync.Mutex
	stoppedSet := map[string]bool{}
	stopInstance := func(inst *instance, crash bool) {
		stopped.Lock()
		if stoppedSet[inst.label] {
			stopped.Unlock()
			return
		}
		stoppedSet[inst.label] = true
		stopped.Unlock()
		if crash {
			inst.lease.suppressRelease.Store(true)
		}
		inst.cancel()
		_ = inst.rt.Stop(context.Background())
	}
	t.Cleanup(func() {
		for _, inst := range instances {
			stopInstance(inst, false)
		}
	})

	instanceForOwner := func(owner string) *instance {
		instanceID, _, _ := strings.Cut(owner, "#")
		for _, inst := range instances {
			if inst.instanceID == instanceID {
				return inst
			}
		}
		return nil
	}
	successorFull := func(inst *instance) bool {
		if inst == nil {
			return false
		}
		health := inst.rt.DeepHealth(context.Background())
		if !health.ReadyForTraffic || health.ServiceLevel != ports.ServiceLevelFull {
			return false
		}
		for _, sessionHealth := range health.Sessions {
			if sessionHealth.SessionID == sessionID {
				return sessionHealth.HasLease && sessionHealth.ServiceLevel == ports.ServiceLevelFull
			}
		}
		return false
	}
	currentOwner := func() (persistence.LeaseInfo, error) { return leaseReader.Current(context.Background(), sessionID) }
	waitForVerifiedOwner := func(desc string) (persistence.LeaseInfo, *instance) {
		var info persistence.LeaseInfo
		var holder *instance
		lrWaitFor(t, 20*time.Second, desc, func() bool {
			current, err := currentOwner()
			if err != nil {
				return false
			}
			candidate := instanceForOwner(current.Owner)
			if candidate == nil || !successorFull(candidate) {
				return false
			}
			info = current
			holder = candidate
			return true
		})
		return info, holder
	}
	waitForSuccessor := func(previous persistence.LeaseInfo, detectedAt time.Time, desc string) (persistence.LeaseInfo, *instance, time.Duration) {
		var info persistence.LeaseInfo
		var successor *instance
		var elapsed time.Duration
		lrWaitFor(t, 30*time.Second, desc, func() bool {
			current, err := currentOwner()
			if err != nil {
				return false
			}
			if current.Owner == previous.Owner || current.Version <= previous.Version {
				return false
			}
			candidate := instanceForOwner(current.Owner)
			if candidate == nil || !successorFull(candidate) {
				return false
			}
			info = current
			successor = candidate
			elapsed = clock.System.Since(detectedAt)
			return true
		})
		return info, successor, elapsed
	}

	mkInstance("A")
	mkInstance("B")
	mkInstance("C")
	gobridgesync(t, 10*time.Second, instances[0].rt, instances[1].rt, instances[2].rt)
	initialLease, initialHolder := waitForVerifiedOwner("initial verified lease owner at ServiceLevelFull")

	sendBulkToSQS(t, sqsInClient, sqsInURL, uc3MsgCount, nil)
	lrWaitFor(t, 60*time.Second, "500 messages before warm failover", func() bool { return collector.count() >= 500 })
	warmDetectedAt := clock.System.Now()
	stopInstance(initialHolder, true)
	warmLease, warmHolder, warmDuration := waitForSuccessor(initialLease, warmDetectedAt, "warm successor owner and fencing version change at ServiceLevelFull")

	lrWaitFor(t, 60*time.Second, "1000 messages before cold failover", func() bool { return collector.count() >= 1000 })
	for _, inst := range instances {
		if inst != warmHolder {
			stopInstance(inst, false)
		}
	}
	verifiedWarmLease, verifiedWarmHolder := waitForVerifiedOwner("verified warm holder before cold replacement")
	require.Equal(t, warmLease.Owner, verifiedWarmLease.Owner)
	require.Equal(t, warmHolder, verifiedWarmHolder)
	coldDetectedAt := clock.System.Now()
	stopInstance(verifiedWarmHolder, true)
	mkInstance("D")
	_, _, coldDuration := waitForSuccessor(verifiedWarmLease, coldDetectedAt, "cold replacement owner and fencing version change at ServiceLevelFull")

	reportUC3FailoverSamples(t, "warm", []time.Duration{warmDuration})
	reportUC3FailoverSamples(t, "cold", []time.Duration{coldDuration})

	// TEST-4: ASSERT the failover objective, not just report it. Under this test's
	// compressed lease profile (LeaseTTL=5s, RenewInterval=400ms) failure-detection
	// to ServiceLevelFull is observed at ~5.1s (warm) / ~5.2s (cold), bounded by the
	// TTL. uc3FailoverSLO is a calibrated ceiling (~3x observed headroom) that a
	// healthy failover always meets but that a regression toward the UNBOUNDED
	// default profile (~336s worst case — the very gap this suite exists to guard)
	// would blow, converting the historical "reported, never gated" duration into a
	// hard pass/fail. It asserts against the MEASURED time to a VERIFIED successor
	// owner + fencing-version change at ServiceLevelFull (waitForSuccessor), not a
	// sleep.
	require.LessOrEqualf(t, warmDuration, uc3FailoverSLO,
		"warm failover to ServiceLevelFull took %s, exceeding the %s objective (regression toward the unbounded default profile?)",
		warmDuration, uc3FailoverSLO)
	require.LessOrEqualf(t, coldDuration, uc3FailoverSLO,
		"cold failover to ServiceLevelFull took %s, exceeding the %s objective", coldDuration, uc3FailoverSLO)

	lrWaitFor(t, 90*time.Second, "2000 messages received", func() bool { return collector.count() >= uc3MsgCount })
	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), uc3MsgCount)
	uniqueIDs := make(map[string]int, len(msgs))
	for _, message := range msgs {
		uniqueIDs[string(message.Payload())]++
	}
	assert.GreaterOrEqual(t, len(uniqueIDs), uc3MsgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

func reportUC3FailoverSamples(t *testing.T, kind string, samples []time.Duration) {
	t.Helper()
	require.NotEmpty(t, samples)
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	percentile := func(p int) time.Duration {
		rank := (p*len(sorted) + 99) / 100
		if rank < 1 {
			rank = 1
		}
		return sorted[rank-1]
	}
	t.Logf("UC3 failure-detection-to-ServiceLevelFull kind=%s samples=%d p50=%s p95=%s p99=%s max=%s", kind, len(sorted), percentile(50), percentile(95), percentile(99), sorted[len(sorted)-1])
}

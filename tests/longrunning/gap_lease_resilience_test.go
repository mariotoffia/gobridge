//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Gap Test: DynamoDB Outage During Lease Renewal (Category 4)
//
// Validates that when the LeaseStore becomes temporarily unavailable,
// the session manager steps down, releases ownership, and a replacement
// runtime acquires the lease once DDB recovers.
// ═══════════════════════════════════════════════════════════════════════════

// faultyLeaseStore wraps a real LeaseStore and can be toggled to fail
// Renew() calls, simulating a DynamoDB outage without killing the
// container (which would also break the OutboxStore).
type faultyLeaseStore struct {
	inner ports.LeaseStore
	fail  atomic.Bool // when true, Renew AND Acquire fail
}

func (f *faultyLeaseStore) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	if f.fail.Load() {
		return persistence.LeaseToken{}, shared.ErrUnavailable.WithMessage("faultyLeaseStore: simulated DDB outage on Acquire")
	}
	return f.inner.Acquire(ctx, leaseID, ownerID, ttl, endpoints)
}

func (f *faultyLeaseStore) Renew(ctx context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	if f.fail.Load() {
		return persistence.LeaseToken{}, shared.ErrUnavailable.WithMessage("faultyLeaseStore: simulated DDB outage on Renew")
	}
	return f.inner.Renew(ctx, leaseID, token, ttl, endpoints)
}

func (f *faultyLeaseStore) Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error {
	return f.inner.Release(ctx, leaseID, token)
}

func (f *faultyLeaseStore) Current(ctx context.Context, leaseID string) (persistence.LeaseInfo, error) {
	return f.inner.Current(ctx, leaseID)
}

// TestGAP_DynamoDBOutage_LeaseRenewal validates that the session manager
// steps down when lease renewal fails and an orchestrator-style replacement
// acquires the lease after recovery.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Bridge running, messages flowing
//	failRenew = true (simulated DDB outage)
//	Lease expires (~2s LeaseTTL)
//	Session steps down (not ready)
//	failRenew = false (DDB recovers)
//	Runtime/session replaced, lease acquired, traffic resumes
//	All 2000 messages delivered
//
// ───────────────────────────────────────────────
//
// Test Parameters:
//   - LeaseTTL: 2s
//   - RenewInterval: 400ms
//   - Messages: 2000
//
// Assertions:
//   - Session becomes not-ready during outage
//   - Session recovers after outage ends
//   - All messages eventually delivered
//   - DLQ empty
func TestGAP_DynamoDBOutage_LeaseRenewal(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		outTopic    = "gap-lr/output"
		testTimeout = 180 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "gap-lr-in")
	realLeaseStore, outboxStore := setupDynamoStores(t)
	fls := &faultyLeaseStore{inner: realLeaseStore}
	dlq := &lrDLQStore{}

	collector := newMQTTCollector(t, outTopic, "gap-lr-col")

	sessID := mqttlocal.UniqueClientID("gap-lr-sess")
	routeCfg := goruntime.RouteConfig{
		ID: "gap-lr-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			AckAfter:     routing.AckAfterOutboxPersist,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "lr-bind", Address: outTopic},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "lr-bind", SessionID: sessID},
		},
	}
	startRuntime := func(instanceID string) *goruntime.Runtime {
		sess := newMQTTSession(t, sessID, connectivity.SessionExclusive)
		snd := setupMQTTSender(t, sess)
		sc := lrSessionConfig(sessID)
		current := goruntime.New(
			goruntime.WithInstanceID(instanceID),
			goruntime.WithLeaseStore(fls),
			goruntime.WithOutboxStore(outboxStore),
			goruntime.WithDLQStore(dlq),
			goruntime.WithLogger(testLogger(t)),
		)
		require.NoError(t, current.AddRoute(
			routeCfg, newSQSReceiver(t, sqsInURL), snd, sess, &sc,
		))
		require.NoError(t, current.Start(ctx))
		t.Cleanup(func() { _ = current.Stop(context.Background()) })
		return current
	}

	rt := startRuntime("gap-lr")
	gobridgesync(t, 15*time.Second, rt)

	t.Logf("GAP-LR: sending %d messages", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait for partial delivery.
	lrWaitFor(t, 30*time.Second, "collector >= 500",
		func() bool { return collector.count() >= 500 })
	t.Logf("GAP-LR: collector at %d — simulating DDB outage", collector.count())

	// Simulate DDB outage: both Acquire and Renew fail.
	fls.fail.Store(true)

	// Poll until the runtime role transitions to "standby", indicating the
	// session lost its lease. LeaseTTL=2s + renewal failures → step-down.
	sawStandby := false
	lrWaitFor(t, 10*time.Second, "role=standby during outage", func() bool {
		role := rt.Role()
		if role == "standby" {
			sawStandby = true
			return true
		}
		return false
	})
	assert.True(t, sawStandby,
		"runtime should enter standby role when DDB is unavailable")
	t.Logf("GAP-LR: session stepped down to standby as expected")

	// Paho sessions are intentionally single-use after lease step-down. Model
	// the documented orchestrator boundary by replacing the runtime and session,
	// rather than asking the closed session to Start again in-process.
	require.NoError(t, rt.Stop(context.Background()))
	fls.fail.Store(false)
	rt = startRuntime("gap-lr-replacement")
	t.Log("GAP-LR: DDB recovered — waiting for replacement runtime")

	// Wait for the replacement bridge to acquire the lease and resume.
	gobridgesync(t, 30*time.Second, rt)
	t.Log("GAP-LR: bridge ready again — waiting for remaining messages")

	// Wait for all messages to be delivered.
	lrWaitFor(t, 90*time.Second,
		fmt.Sprintf("collector >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })

	delivered := collector.count()
	t.Logf("GAP-LR: delivered=%d/%d, dlq=%d", delivered, msgCount, dlq.count())

	assert.GreaterOrEqual(t, delivered, msgCount,
		"all %d messages should be delivered after DDB recovery", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

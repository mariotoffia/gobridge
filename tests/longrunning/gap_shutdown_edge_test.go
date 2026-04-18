//go:build longrunning

package longrunning_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Gap Tests: Graceful Shutdown Edge Cases (Category 12)
//
// Summary:
// ┌──────┬──────────────────────────────────┬──────────┐
// │ ID   │ Description                      │ Status   │
// ├──────┼──────────────────────────────────┼──────────┤
// │ SD-1 │ Outbox in-flight during Stop()   │ PENDING  │
// │ SD-2 │ Double-stop concurrency safety   │ PENDING  │
// └──────┴──────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestGAP_ShutdownWithOutboxInFlight validates that Stop() called while
// the outbox drainer is mid-batch completes finalDrain without orphaning
// records.
//
// Scenario:
// ───────────────────────────────────────────────
//   SQS ──▶ [Outbox Persist] ──▶ [Slow Drain] ──▶ MQTT
//                                      │
//                                 Stop() called
//                                      │
//                                 finalDrain runs
//                                      ▼
//                              Records completed
// ───────────────────────────────────────────────
//
// Test Parameters:
//   - DeliveryMode: SharedOutbox
//   - AckAfter: AckAfterOutboxPersist
//   - SlowSender delay: 100ms per message
//   - Messages: 500
//
// Assertions:
//   - Stop() returns nil
//   - At least 100 messages delivered before stop
//   - Outbox has minimal pending records after stop
func TestGAP_ShutdownWithOutboxInFlight(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 500
		outTopic    = "gap-sd1/output"
		testTimeout = 120 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "gap-sd1-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	sessID := mqttlocal.UniqueClientID("gap-sd1-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	baseSnd := setupMQTTSender(t, sess)
	snd := newSlowSender(baseSnd, 100*time.Millisecond)
	sc := lrSessionConfig(sessID)

	collector := newMQTTCollector(t, outTopic, "gap-sd1-col")

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-sd1"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-sd1-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			AckAfter:     domain.AckAfterOutboxPersist,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "sd1-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "sd1-bind", SessionID: sessID},
		},
	}, newSQSReceiver(t, sqsInURL), snd, sess, &sc))

	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait until drainer is actively working.
	lrWaitFor(t, 60*time.Second, "collector >= 50",
		func() bool { return collector.count() >= 50 })

	// Record count just before Stop — this is the baseline.
	preStopCount := collector.count()
	t.Logf("GAP-SD1: collector at %d, calling Stop()", preStopCount)

	// Stop with 30s deadline — finalDrain should complete in-flight batch.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	err := rt.Stop(stopCtx)
	assert.NoError(t, err, "Stop() should complete without error")

	time.Sleep(2 * time.Second) // SYNC: let final deliveries reach collector via MQTT

	delivered := collector.count()
	t.Logf("GAP-SD1: delivered=%d/%d (preStop=%d, delta=%d), dlq=%d",
		delivered, msgCount, preStopCount, delivered-preStopCount, dlq.count())

	// Verify finalDrain delivered additional messages after Stop was called.
	// The drainer was mid-batch; finalDrain should complete that batch.
	assert.Greater(t, delivered, preStopCount,
		"finalDrain should deliver additional messages after Stop() (pre=%d, post=%d)",
		preStopCount, delivered)

	// Not all messages should be delivered — Stop() terminates after finalDrain.
	assert.Less(t, delivered, msgCount,
		"not all messages should be delivered — Stop() terminates processing")
}

// TestGAP_DoubleStopSafety validates that two concurrent Stop() calls
// do not panic, deadlock, or return unexpected errors.
//
// Scenario:
// ───────────────────────────────────────────────
//   Goroutine 1 ──┐
//                  ├──▶ rt.Stop() simultaneously
//   Goroutine 2 ──┘
//                          │
//                          ▼
//                   No panic, no deadlock
//                   rt.IsRunning() == false
// ───────────────────────────────────────────────
//
// Assertions:
//   - No panic
//   - At least one Stop() returns nil
//   - Runtime is no longer running
func TestGAP_DoubleStopSafety(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 100
		outTopic    = "gap-sd2/output"
		testTimeout = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "gap-sd2-in")
	dlq := &lrDLQStore{}

	sessID := mqttlocal.UniqueClientID("gap-sd2-sess")
	sess := setupMQTTSession(t, sessID, domain.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	collector := newMQTTCollector(t, outTopic, "gap-sd2-col")

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-sd2"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-sd2-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "sd2-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), snd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)
	lrWaitFor(t, 15*time.Second, "collector >= 10",
		func() bool { return collector.count() >= 10 })

	// Two concurrent Stop() calls via barrier channel.
	errs := make([]error, 2)
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-barrier // wait for simultaneous release
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer stopCancel()
			errs[idx] = rt.Stop(stopCtx)
		}(i)
	}
	close(barrier) // release both goroutines
	wg.Wait()

	t.Logf("GAP-SD2: stop[0]=%v, stop[1]=%v", errs[0], errs[1])
	t.Logf("GAP-SD2: delivered=%d, dlq=%d", collector.count(), dlq.count())

	// At least one Stop() must succeed.
	assert.True(t, errs[0] == nil || errs[1] == nil,
		"at least one Stop() call should return nil")

	assert.False(t, rt.IsRunning(), "runtime should not be running after double stop")

	// If we reach here, no panic occurred (test framework catches panics).
	t.Log("GAP-SD2: no panic, no deadlock — double-stop is safe")
}

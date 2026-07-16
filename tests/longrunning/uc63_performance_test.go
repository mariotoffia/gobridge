//go:build longrunning

package longrunning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// =========================================================================
// UC63: Memory Stability
//
// Sends messages through SharedOutbox and verifies that heap usage
// returns to baseline after processing — no leaks, no unbounded growth.
//
// Strategy: run a warmup batch to stabilize allocations, take a GC'd
// baseline, run the real load, wait for quiescence, take another GC'd
// reading, and assert the DELTA is bounded. This avoids absolute heap
// thresholds that are meaningless under variable Docker resource pressure.
//
// Assert: Heap growth (final - baseline) < 50MB after full quiescence.
// =========================================================================

func TestUC63_MemoryStability(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		warmupCount = 1000
		msgCount    = 10000
		outTopic    = "uc63/output"
		testTimeout = 360 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc63-in")
	leaseStore := memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))
	outboxClock := clocktest.New()
	outboxStore := memoryoutbox.NewStore(
		memoryoutbox.WithClock(outboxClock),
		memoryoutbox.WithRetention(time.Second),
	)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc63-col")

	sessID := mqttlocal.UniqueClientID("uc63-sess")
	sess := newMQTTSession(t, sessID, connectivity.SessionExclusive)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc63-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc63-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			MaxInFlight:  200,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc63-bind", Address: outTopic},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc63-bind", SessionID: sessID},
		},
	}, rx, snd, sess, &sc))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	compactOutbox := func() {
		t.Helper()
		require.NoError(t, rt.WaitQuiescent(ctx, goruntime.QuiescenceOptions{
			MinQuiet: 500 * time.Millisecond,
			Timeout:  10 * time.Second,
		}))
		lrWaitFor(t, 10*time.Second, "outbox pending depth == 0", func() bool {
			pending, supported, err := rt.OutboxPending(
				ctx, persistence.OutboxPartitionKey(sessID, ""),
			)
			return err == nil && supported && pending == 0
		})
		// The in-memory store retains terminal records for deduplication. Advance
		// its test clock beyond retention and trigger compaction so this measures
		// runtime memory after persistence has released completed payloads.
		outboxClock.Advance(2 * time.Minute)
		_, err := outboxStore.Expire(
			ctx, outboxClock.Now(), persistence.OutboxPartitionKey(sessID, ""),
		)
		require.NoError(t, err)
	}

	// Phase 1: warmup — stabilize goroutine stacks, connection pools,
	// DDB table caches, and GC pacing. Results are discarded.
	t.Logf("UC63: warmup — sending %d messages", warmupCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, warmupCount, nil)
	lrWaitFor(t, 60*time.Second,
		fmt.Sprintf("warmup >= %d", warmupCount),
		func() bool { return countUnique(collector) >= warmupCount })
	compactOutbox()

	// Phase 2: baseline — force multiple GC cycles to get a clean reading.
	baseline := stableHeapAlloc()
	t.Logf("UC63: baseline heap=%dMB (after warmup + GC)", baseline/(1<<20))

	// Phase 3: load — send the real batch.
	t.Logf("UC63: sending %d messages", msgCount)
	totalExpected := warmupCount + msgCount
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 280*time.Second,
		fmt.Sprintf("unique >= %d", totalExpected),
		func() bool { return countUnique(collector) >= totalExpected })

	// Phase 4: quiescence — let persistence release terminal payloads, then
	// force multiple GC cycles.
	compactOutbox()
	final := stableHeapAlloc()

	t.Logf("UC63: heap — baseline=%dMB, final=%dMB, delta=%dMB",
		baseline/(1<<20), final/(1<<20), int64(final-baseline)/(1<<20))
	t.Logf("UC63: unique=%d, total=%d, dlq=%d",
		countUnique(collector), collector.count(), dlq.count())

	// Assert: heap growth should be bounded. After all messages are
	// processed and buffers drained, retained memory should not grow
	// proportionally to throughput. A 50MB allowance covers goroutine
	// stacks, runtime overhead, and measurement noise.
	const maxGrowth = 50 << 20 // 50MB
	var growth uint64
	if final > baseline {
		growth = final - baseline
	}
	assert.LessOrEqual(t, growth, uint64(maxGrowth),
		"Heap growth (%dMB) after processing %d messages should be < %dMB — potential memory leak",
		growth/(1<<20), msgCount, maxGrowth/(1<<20))

	require.GreaterOrEqual(t, countUnique(collector), totalExpected,
		"All %d messages must be delivered", totalExpected)
}

// =========================================================================
// UC64: Latency Percentiles
//
// Sends 10,000 messages through DirectHold with a latencyRecorder
// processor and measures P50/P95/P99 send latencies.
//
// Assert: P50 < 500ms, P95 < 2s, P99 < 5s.
// =========================================================================

func TestUC64_LatencyPercentiles(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 10000
		outTopic    = "uc64/output"
		testTimeout = 240 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc64-in")
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc64-col")

	sessID := mqttlocal.UniqueClientID("uc64-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionExclusive)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)

	lr := &latencyRecorder{}

	rt := goruntime.New(
		goruntime.WithInstanceID("uc64-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc64-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  100,
		},
		Processors: []ports.Processor{lr},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc64-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, rx, snd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC64: sending %d messages", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 220*time.Second,
		fmt.Sprintf("collector >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })

	p50 := lr.percentile(0.50)
	p95 := lr.percentile(0.95)
	p99 := lr.percentile(0.99)

	t.Logf("UC64: latency — P50=%v, P95=%v, P99=%v (samples=%d)",
		p50, p95, p99, lr.count())
	t.Logf("UC64: delivered=%d, dlq=%d", collector.count(), dlq.count())

	require.Less(t, p50, 500*time.Millisecond,
		"P50 latency must be < 500ms (got %v)", p50)
	require.Less(t, p95, 2*time.Second,
		"P95 latency must be < 2s (got %v)", p95)
	require.Less(t, p99, 5*time.Second,
		"P99 latency must be < 5s (got %v)", p99)
}

// =========================================================================
// UC66: Multi-Tenant Isolation
//
// 10 tenants × 500 messages = 5,000 total. Tenant 0 has a slow
// processor (200ms per message). Other tenants should not be blocked.
//
// Assert: Tenants 1-9 each complete in < 30s. Tenant 0 in < 120s.
// =========================================================================

func TestUC66_MultiTenantIsolation(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		tenants     = 10
		perTenant   = 500
		msgCount    = tenants * perTenant
		outTopic    = "uc66/output"
		testTimeout = 240 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc66-in")
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc66-col")

	sessID := mqttlocal.UniqueClientID("uc66-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionExclusive)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc66-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc66-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  100,
		},
		Processors: []ports.Processor{
			&tenantSlowProcessor{delay: 200 * time.Millisecond, slowTenant: "0"},
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc66-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, rx, snd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	// Send messages round-robin across tenants (msg N → tenant N%10).
	t.Logf("UC66: sending %d messages (%d tenants × %d each, tenant 0 slow=200ms)",
		msgCount, tenants, perTenant)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, func(i int) map[string]string {
		return map[string]string{"tenant_id": fmt.Sprintf("%d", i%tenants)}
	})

	// Track per-tenant completion by polling the collector.
	type seqMsg struct {
		Seq int `json:"seq"`
	}
	start := time.Now()
	tenantDone := make(map[int]time.Duration)

	for time.Since(start) < 220*time.Second {
		msgs := collector.getMessages()
		counts := make(map[int]int)
		for _, m := range msgs {
			var sm seqMsg
			if json.Unmarshal(m.Payload(), &sm) == nil {
				counts[sm.Seq%tenants]++
			}
		}

		allDone := true
		for tid := 0; tid < tenants; tid++ {
			if counts[tid] >= perTenant {
				if _, ok := tenantDone[tid]; !ok {
					tenantDone[tid] = time.Since(start)
					t.Logf("UC66: tenant %d completed at %v (%d msgs)",
						tid, tenantDone[tid].Round(time.Millisecond), counts[tid])
				}
			} else {
				allDone = false
			}
		}
		if allDone {
			break
		}
		time.Sleep(500 * time.Millisecond) // SYNC: poll for per-tenant completion
	}

	// Log and assert per-tenant timing.
	for tid := 0; tid < tenants; tid++ {
		dur, ok := tenantDone[tid]
		if !ok {
			t.Errorf("UC66: tenant %d did not complete (check header propagation)", tid)
			continue
		}
		if tid == 0 {
			assert.Less(t, dur, 120*time.Second,
				"Slow tenant 0 should complete in < 120s")
		} else {
			assert.Less(t, dur, 30*time.Second,
				"Tenant %d should complete in < 30s (isolation from tenant 0)", tid)
		}
	}

	t.Logf("UC66: total delivered=%d, dlq=%d", collector.count(), dlq.count())
	require.GreaterOrEqual(t, collector.count(), msgCount,
		"All %d messages must be delivered", msgCount)
}

// =========================================================================
// UC65: Throughput Ceiling Discovery
//
// 4 batches of increasing size with MaxInFlight=1000.
// Discovers the maximum sustainable throughput.
// =========================================================================

func TestUC65_ThroughputCeiling(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		outTopic    = "uc65/output"
		testTimeout = 480 * time.Second
	)

	batches := []int{500, 1000, 2000, 3000}
	totalMsgs := 0
	for _, b := range batches {
		totalMsgs += b
	}

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc65-in")
	leaseStore := memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))
	outboxStore := memoryoutbox.NewStore()
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc65-col")

	sessID := mqttlocal.UniqueClientID("uc65-sess")
	sess := newMQTTSession(t, sessID, connectivity.SessionExclusive)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc65-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc65-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			MaxInFlight:  1000,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc65-bind", Address: outTopic},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc65-bind", SessionID: sessID},
		},
	}, rx, snd, sess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	delivered := 0
	for _, batchSize := range batches {
		start := time.Now()
		t.Logf("UC65: sending batch of %d (total so far: %d)", batchSize, delivered)
		sendBulkToSQS(t, sqsInClient, sqsInURL, batchSize, nil)
		delivered += batchSize

		lrWaitFor(t, 90*time.Second,
			fmt.Sprintf("unique >= %d", delivered),
			func() bool { return countUnique(collector) >= delivered })

		elapsed := time.Since(start)
		rate := float64(batchSize) / elapsed.Seconds()
		t.Logf("UC65: batch %d done in %v (%.0f msgs/sec)", batchSize, elapsed, rate)
	}

	unique := countUnique(collector)
	t.Logf("UC65: total unique=%d, total=%d, dlq=%d", unique, collector.count(), dlq.count())
	require.GreaterOrEqual(t, unique, totalMsgs)
	assert.Equal(t, 0, dlq.count())
}

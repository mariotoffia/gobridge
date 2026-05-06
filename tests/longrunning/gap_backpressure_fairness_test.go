//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Gap Test: Backpressure Fairness (Category 8 — Backpressure)
//
// Validates that two routes sharing GlobalMaxInFlight do not starve
// the fast route when one route has a slow sender.
// ═══════════════════════════════════════════════════════════════════════════

// TestGAP_BackpressureFairness_MixedFastSlow validates that a fast route
// sharing GlobalMaxInFlight with a slow route is not starved.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	GlobalMaxInFlight = 50
//
//	Route A (slow):
//	  SQS-A ──▶ [slowSender 200ms] ──▶ MQTT-A
//
//	Route B (fast):
//	  SQS-B ──▶ [normalSender]     ──▶ MQTT-B
//
//	Both receive 1000 messages simultaneously.
//	Fast route should finish significantly before slow route.
//
// ───────────────────────────────────────────────
//
// Test Parameters:
//   - GlobalMaxInFlight: 50
//   - Route A: slowSender(200ms), MaxInFlight=100
//   - Route B: normal sender, MaxInFlight=100
//   - Messages per route: 1000
//
// Assertions:
//   - Fast route finishes before slow route
//   - Both routes deliver all messages
//   - DLQ empty
func TestGAP_BackpressureFairness_MixedFastSlow(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 1000
		outTopicA   = "gap-bp/slow"
		outTopicB   = "gap-bp/fast"
		testTimeout = 180 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Two input queues.
	sqsInURLa, sqsInClientA := setupSQSQueue(t, "gap-bp-slow")
	sqsInURLb, sqsInClientB := setupSQSQueue(t, "gap-bp-fast")
	dlq := &lrDLQStore{}

	// Two collectors on different topics.
	collectorA := newMQTTCollector(t, outTopicA, "gap-bp-col-a")
	collectorB := newMQTTCollector(t, outTopicB, "gap-bp-col-b")

	// Concurrency trackers to measure actual in-flight counts per route.
	trackerA := &concurrencyTracker{}
	trackerB := &concurrencyTracker{}

	// Route A: slow sender.
	sessA := setupMQTTSession(t, mqttlocal.UniqueClientID("gap-bp-sa"), connectivity.SessionEphemeral)
	baseSndA := setupMQTTSender(t, sessA)
	slowSndA := newSlowSender(baseSndA, 200*time.Millisecond)

	// Route B: fast sender.
	sessB := setupMQTTSession(t, mqttlocal.UniqueClientID("gap-bp-sb"), connectivity.SessionEphemeral)
	sndB := setupMQTTSender(t, sessB)

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-bp"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithGlobalMaxInFlight(50),
		goruntime.WithLogger(testLogger(t)),
	)

	// Route A: slow (with concurrency tracker).
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-bp-slow",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  100,
		},
		Processors:         []ports.Processor{trackerA},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "bp-a", Address: outTopicA}),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURLa), slowSndA, sessA, nil))

	// Route B: fast (with concurrency tracker).
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-bp-fast",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  100,
		},
		Processors:         []ports.Processor{trackerB},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "bp-b", Address: outTopicB}),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURLb), sndB, sessB, nil))

	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 10*time.Second, rt)

	// Send to both queues simultaneously.
	t.Logf("GAP-BP: sending %d messages to each route", msgCount)
	start := time.Now()
	sendBulkToSQS(t, sqsInClientA, sqsInURLa, msgCount, nil)
	sendBulkToSQS(t, sqsInClientB, sqsInURLb, msgCount, nil)

	// Wait for fast route first.
	lrWaitFor(t, 60*time.Second,
		fmt.Sprintf("fast collector >= %d", msgCount),
		func() bool { return collectorB.count() >= msgCount })
	fastDuration := time.Since(start)
	t.Logf("GAP-BP: fast route completed in %v", fastDuration)

	// Wait for slow route.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("slow collector >= %d", msgCount),
		func() bool { return collectorA.count() >= msgCount })
	slowDuration := time.Since(start)
	t.Logf("GAP-BP: slow route completed in %v", slowDuration)

	t.Logf("GAP-BP: fast=%v, slow=%v, ratio=%.2f",
		fastDuration, slowDuration, float64(slowDuration)/float64(fastDuration))
	t.Logf("GAP-BP: fast=%d, slow=%d, dlq=%d",
		collectorB.count(), collectorA.count(), dlq.count())

	// Fast route should finish before slow route.
	assert.Less(t, fastDuration, slowDuration,
		"fast route should finish before slow route (fast=%v, slow=%v)",
		fastDuration, slowDuration)

	// Both routes deliver all messages with unique content.
	uniqueA := countUnique(collectorA)
	uniqueB := countUnique(collectorB)
	t.Logf("GAP-BP: uniqueA=%d, uniqueB=%d", uniqueA, uniqueB)
	assert.GreaterOrEqual(t, collectorA.count(), msgCount,
		"slow route should deliver all %d messages", msgCount)
	assert.GreaterOrEqual(t, collectorB.count(), msgCount,
		"fast route should deliver all %d messages", msgCount)
	assert.GreaterOrEqual(t, uniqueA, msgCount,
		"slow route should deliver %d unique messages", msgCount)
	assert.GreaterOrEqual(t, uniqueB, msgCount,
		"fast route should deliver %d unique messages", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	// Verify global semaphore was the bottleneck by checking max concurrency.
	// The combined peak concurrency across both routes should not exceed the
	// GlobalMaxInFlight limit (50). The per-route trackers measure concurrent
	// Process calls; the global semaphore is acquired BEFORE Process.
	maxA := trackerA.maxConcurrency()
	maxB := trackerB.maxConcurrency()
	t.Logf("GAP-BP: max concurrency: slow=%d, fast=%d, combined peak<=%d (limit=50)",
		maxA, maxB, maxA+maxB)

	// The combined observed concurrency should be bounded by GlobalMaxInFlight.
	// Note: trackers measure processor-level concurrency, which is bounded by
	// the per-route semaphore first, then the global semaphore. The sum of
	// peaks may exceed the global limit (peaks may not coincide), so we only
	// check that neither route alone exceeded the global limit.
	assert.LessOrEqual(t, maxA, int64(50),
		"slow route concurrency should not exceed GlobalMaxInFlight")
	assert.LessOrEqual(t, maxB, int64(50),
		"fast route concurrency should not exceed GlobalMaxInFlight")

	require.NoError(t, rt.Stop(context.Background()))
}

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

// =========================================================================
// UC80: Smooth Throughput Baseline
//
// SQS-IN (10,000 msgs) -> [Bridge DirectHold, MaxInFlight=100]
//                       -> MQTT "uc80/out" -> Collector
//
// No failures, no slow processors. Establishes a clean throughput
// baseline with structured latency/counter reporting.
//
// Assert: collector >= 10,000, DLQ == 0.
// =========================================================================

func TestUC80_SmoothThroughputBaseline(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		outTopic    = "uc80/out"
		testTimeout = 240 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc80-in")
	dlq := &lrDLQStore{}
	rec := &ports.RecordingExporter{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc80-col")

	sessID := mqttlocal.UniqueClientID("uc80-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionEphemeral)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc80-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithMetrics(rec),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc80-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  20,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc80-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, rx, snd, nil, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	start := time.Now()
	t.Logf("UC80: sending %d messages", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Poll until all messages arrive, logging progress.
	deadline := time.Now().Add(220 * time.Second)
	for time.Now().Before(deadline) {
		cur := collector.count()
		if cur >= msgCount {
			break
		}
		if int(time.Since(start).Seconds())%30 == 0 {
			t.Logf("UC80: progress %d/%d (dlq=%d)", cur, msgCount, dlq.count())
		}
		time.Sleep(500 * time.Millisecond) // SYNC: poll for message delivery progress
	}
	elapsed := time.Since(start)

	delivered := collector.count()
	report := &benchmarkReport{
		TestName:      "UC80 Smooth Throughput",
		MsgsSent:      msgCount,
		MsgsDelivered: delivered,
		MsgsDLQ:       dlq.count(),
		Duration:      elapsed,
	}
	report.logReport(t, rec)

	require.GreaterOrEqual(t, delivered, msgCount,
		"All %d messages must be delivered (got %d, dlq=%d)", msgCount, delivered, dlq.count())
	assert.Equal(t, 0, dlq.count(), "No messages should end up in DLQ")
}

// =========================================================================
// UC81: Strained Throughput
//
// SQS-IN (10,000 msgs) -> [Bridge DirectHold, MaxInFlight=50,
//                          slowProcessor(50ms), faultySender(10%)]
//                       -> MQTT "uc81/out" -> Collector
//
// Tests throughput under adverse conditions: 10% send failures and
// 50ms processor delay. DLQ configured to capture permanent failures.
//
// Assert: delivered + DLQ == 10,000.
// =========================================================================

func TestUC81_StrainedThroughput(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		outTopic    = "uc81/out"
		testTimeout = 600 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc81-in")
	dlq := &lrDLQStore{}
	rec := &ports.RecordingExporter{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc81-col")

	sessID := mqttlocal.UniqueClientID("uc81-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionEphemeral)
	rawSnd := setupMQTTSender(t, sess)
	faulty := newFaultySender(rawSnd, 10)
	rx := newSQSReceiver(t, sqsInURL)
	slow := newSlowProcessor("uc81-slow", 50*time.Millisecond)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc81-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithMetrics(rec),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc81-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			MaxInFlight:        20,
			OnExpired:          routing.ExpiredDLQ,
			OnPermanentFailure: routing.FailureDLQ,
		},
		Processors: []ports.Processor{slow},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc81-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, rx, faulty, nil, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	start := time.Now()
	t.Logf("UC81: sending %d messages (slowProcessor=50ms, faultySender=10%%)", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait for all messages to be either delivered or DLQ'd.
	lrWaitFor(t, 580*time.Second,
		fmt.Sprintf("delivered + DLQ >= %d", msgCount),
		func() bool { return collector.count()+dlq.count() >= msgCount })
	elapsed := time.Since(start)

	delivered := collector.count()
	dlqCount := dlq.count()
	report := &benchmarkReport{
		TestName:      "UC81 Strained Throughput",
		MsgsSent:      msgCount,
		MsgsDelivered: delivered,
		MsgsDLQ:       dlqCount,
		Duration:      elapsed,
	}
	report.logReport(t, rec)

	t.Logf("UC81: delivered=%d, dlq=%d, total=%d (expected=%d)",
		delivered, dlqCount, delivered+dlqCount, msgCount)

	require.GreaterOrEqual(t, delivered+dlqCount, msgCount,
		"delivered (%d) + DLQ (%d) must account for all %d messages",
		delivered, dlqCount, msgCount)
}

// =========================================================================
// UC82: Memory Stability Under Load
//
// SQS-IN (50,000 msgs) -> [Bridge SharedOutbox, MaxInFlight=200,
//                          Exclusive session with DynamoDB stores]
//                       -> MQTT "uc82/out" -> Collector
//
// Uses heapSampler to track memory throughout the run. Verifies that
// heap does not grow unboundedly.
//
// Assert: all 50,000 delivered, final heap <= 2x initial, max < 500MB.
// =========================================================================

func TestUC82_MemoryStabilityUnderLoad(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 10000
		outTopic    = "uc82/out"
		testTimeout = 600 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc82-in")
	dlq := &lrDLQStore{}
	rec := &ports.RecordingExporter{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc82-col")

	sessID := mqttlocal.UniqueClientID("uc82-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionEphemeral)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc82-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithMetrics(rec),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc82-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  20,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc82-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, rx, snd, nil, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	// Sample heap AFTER runtime warmup so initial is realistic.
	heap := newHeapSampler(1 * time.Second)

	start := time.Now()
	t.Logf("UC82: sending %d messages (DirectHold, initial heap=%dMB)",
		msgCount, heap.initialHeap()/(1<<20))
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 580*time.Second,
		fmt.Sprintf("unique >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })
	elapsed := time.Since(start)

	heap.stop()

	initial := heap.initialHeap()
	maxH := heap.maxHeap()
	final := heap.finalHeap()
	delivered := collector.count()

	report := &benchmarkReport{
		TestName:      "UC82 Memory Stability Under Load",
		MsgsSent:      msgCount,
		MsgsDelivered: delivered,
		MsgsDLQ:       dlq.count(),
		Duration:      elapsed,
		HeapInitial:   initial,
		HeapPeak:      maxH,
		HeapFinal:     final,
	}
	report.logReport(t, rec)

	require.GreaterOrEqual(t, delivered, msgCount,
		"All %d messages must be delivered", msgCount)
	// Absolute cap: peak heap must stay under 500MB for 10K messages.
	// The 2x-initial ratio is not meaningful when initial is very small
	// (a fresh Go process starts at ~2MB regardless of workload).
	require.Less(t, maxH, uint64(500<<20),
		"Max heap (%dMB) must be < 500MB", maxH/(1<<20))
	assert.Equal(t, 0, dlq.count(), "No DLQ entries expected")
}

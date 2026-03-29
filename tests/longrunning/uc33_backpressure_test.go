//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// =========================================================================
// UC33: MaxInFlight=1 Serial Processing
//
// 500 messages with MaxInFlight=1. concurrencyTracker verifies that the
// maximum observed concurrency is exactly 1.
// =========================================================================

func TestUC33_MaxInFlight1_Serial(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 500
		pollTimeout  = 120 * time.Second
	)

	inQueueURL, inClient := setupSQSQueue(t, "uc33-in")
	collector := newMQTTCollector(t, "uc33/output/data", "uc33-col")
	time.Sleep(300 * time.Millisecond)

	dlqStore := &lrDLQStore{}
	tracker := &concurrencyTracker{}

	sess := setupMQTTSession(t, uniqueID("uc33-bridge"), domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inQueueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc33-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc33-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        1,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Processors: []ports.Processor{tracker},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc33/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSender, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	sendBulkToSQS(t, inClient, inQueueURL, msgCount, nil)

	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector >= %d", msgCount), func() bool {
		return collector.count() >= msgCount
	})

	time.Sleep(1 * time.Second)

	gotMax := tracker.maxConcurrency()
	require.Equal(t, int64(1), gotMax,
		"max concurrency should be 1, got %d", gotMax)

	require.GreaterOrEqual(t, collector.count(), msgCount)

	t.Logf("UC33: MaxInFlight=1, maxConcurrency=%d, collected=%d",
		gotMax, collector.count())
}

// =========================================================================
// UC34: MaxInFlight=1000 High Concurrency
//
// 10,000 messages with MaxInFlight=1000. concurrencyTracker verifies that
// maxConcurrency > 1 (parallel processing occurs).
// =========================================================================

func TestUC34_MaxInFlight1000_HighConcurrency(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 10000
		pollTimeout  = 180 * time.Second
	)

	inQueueURL, inClient := setupSQSQueue(t, "uc34-in")
	collector := newMQTTCollector(t, "uc34/output/data", "uc34-col")
	time.Sleep(300 * time.Millisecond)

	dlqStore := &lrDLQStore{}
	tracker := &concurrencyTracker{}

	sess := setupMQTTSession(t, uniqueID("uc34-bridge"), domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inQueueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc34-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc34-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        1000,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Processors: []ports.Processor{tracker},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc34/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSender, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	sendBulkToSQS(t, inClient, inQueueURL, msgCount, nil)

	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector >= %d", msgCount), func() bool {
		return collector.count() >= msgCount
	})

	time.Sleep(2 * time.Second)

	gotMax := tracker.maxConcurrency()
	require.Greater(t, gotMax, int64(1),
		"max concurrency should be > 1 with MaxInFlight=1000, got %d", gotMax)

	require.GreaterOrEqual(t, collector.count(), msgCount)

	t.Logf("UC34: MaxInFlight=1000, maxConcurrency=%d, collected=%d",
		gotMax, collector.count())
}

// =========================================================================
// UC35: GlobalMaxInFlight Across Three Routes
//
// 3 routes, each SQS->MQTT, sharing GlobalMaxInFlight(50).
// Shared concurrencyTracker. Send 1,000 per route.
// Verify total concurrent <= 50.
// =========================================================================

func TestUC35_GlobalMaxInFlight_ThreeRoutes(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		perRoute = 1000
		pollTimeout  = 120 * time.Second
		globalMF = 50
	)

	dlqStore := &lrDLQStore{}
	tracker := &concurrencyTracker{}

	rt := goruntime.New(
		goruntime.WithInstanceID("uc35-bridge"),
		goruntime.WithDLQStore(dlqStore),
		goruntime.WithGlobalMaxInFlight(globalMF),
	)

	type routeSetup struct {
		queueURL string
		client   *awssqs.Client
		topic    string
	}

	var routes []routeSetup

	for i := 0; i < 3; i++ {
		label := fmt.Sprintf("uc35-r%d", i)
		inURL, inClient := setupSQSQueue(t, label+"-in")
		topic := fmt.Sprintf("uc35/output/%d", i)

		sess := setupMQTTSession(t, uniqueID(label), domain.SessionEphemeral)
		mqttSnd := setupMQTTSender(t, sess)
		sqsRx := newSQSReceiver(t, inURL)

		routeCfg := goruntime.RouteConfig{
			ID: label,
			Policy: domain.RoutePolicy{
				DeliveryMode:       domain.DeliveryDirectHold,
				MaxInFlight:        100,
				OnPermanentFailure: domain.FailureDLQ,
			},
			Processors: []ports.Processor{tracker},
			Resolver: goruntime.NewStaticResolver(
				domain.DispatchPlan{BindingID: label + "-out", Address: topic},
			),
			SourceCapabilities: directHoldCaps,
		}
		require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, nil, nil))
		routes = append(routes, routeSetup{queueURL: inURL, client: inClient, topic: topic})
	}

	// One shared collector on a wildcard topic.
	collector := newMQTTCollector(t, "uc35/output/+", "uc35-col")
	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	// Send messages to all three queues.
	for _, r := range routes {
		sendBulkToSQS(t, r.client, r.queueURL, perRoute, nil)
	}

	totalExpected := perRoute * 3
	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector >= %d", totalExpected), func() bool {
		return collector.count() >= totalExpected
	})

	time.Sleep(2 * time.Second)

	gotMax := tracker.maxConcurrency()
	require.LessOrEqual(t, gotMax, int64(globalMF),
		"max concurrency should be <= %d (global limit), got %d",
		globalMF, gotMax)

	require.GreaterOrEqual(t, collector.count(), totalExpected)

	t.Logf("UC35: GlobalMaxInFlight=%d, maxConcurrency=%d, collected=%d",
		globalMF, gotMax, collector.count())
}

// =========================================================================
// UC36: Slow Consumer
//
// 1,000 messages with slowSender(100ms) and MaxInFlight=20.
// All 1,000 arrive. No OOM.
// =========================================================================

func TestUC36_SlowConsumer(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 1000
		pollTimeout  = 180 * time.Second
	)

	inQueueURL, inClient := setupSQSQueue(t, "uc36-in")
	collector := newMQTTCollector(t, "uc36/output/data", "uc36-col")
	time.Sleep(300 * time.Millisecond)

	dlqStore := &lrDLQStore{}

	sess := setupMQTTSession(t, uniqueID("uc36-bridge"), domain.SessionEphemeral)
	realSender := setupMQTTSender(t, sess)
	slow := newSlowSender(realSender, 100*time.Millisecond)
	sqsRx := newSQSReceiver(t, inQueueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc36-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc36-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        20,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc36/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, slow, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	sendBulkToSQS(t, inClient, inQueueURL, msgCount, nil)

	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector >= %d", msgCount), func() bool {
		return collector.count() >= msgCount
	})

	time.Sleep(2 * time.Second)

	got := collector.count()
	require.GreaterOrEqual(t, got, msgCount,
		"collector should have >= %d messages, got %d", msgCount, got)

	assert.Equal(t, 0, dlqStore.count(), "DLQ should be empty")

	t.Logf("UC36: slowSender(100ms), MaxInFlight=20, collected=%d, DLQ=%d",
		got, dlqStore.count())
}

// =========================================================================
// UC37: Burst Then Idle
//
// 3 bursts of 1,000 messages with 5s gap between each.
// Verify 3,000 total. Bridge healthy after each burst.
// =========================================================================

func TestUC37_BurstThenIdle(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		burstSize  = 1000
		burstCount = 3
		gapSeconds = 5
		pollTimeout    = 180 * time.Second
	)

	inQueueURL, inClient := setupSQSQueue(t, "uc37-in")
	collector := newMQTTCollector(t, "uc37/output/data", "uc37-col")
	time.Sleep(300 * time.Millisecond)

	dlqStore := &lrDLQStore{}

	sess := setupMQTTSession(t, uniqueID("uc37-bridge"), domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inQueueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc37-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc37-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        100,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc37/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSender, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	totalExpected := 0
	for burst := 0; burst < burstCount; burst++ {
		t.Logf("UC37: sending burst %d (%d messages)", burst+1, burstSize)
		sendBulkToSQS(t, inClient, inQueueURL, burstSize,
			func(i int) map[string]string {
				return nil
			},
		)
		totalExpected += burstSize

		// Wait for this burst to be received before checking health.
		lrWaitFor(t, 60*time.Second,
			fmt.Sprintf("collector >= %d after burst %d", totalExpected, burst+1),
			func() bool {
				return collector.count() >= totalExpected
			},
		)

		require.True(t, rt.Healthy(),
			"bridge should be healthy after burst %d", burst+1)

		if burst < burstCount-1 {
			t.Logf("UC37: idle gap %ds before next burst", gapSeconds)
			time.Sleep(time.Duration(gapSeconds) * time.Second)
		}
	}

	time.Sleep(2 * time.Second)

	got := collector.count()
	total := burstSize * burstCount
	require.GreaterOrEqual(t, got, total,
		"collector should have >= %d messages, got %d", total, got)

	assert.Equal(t, 0, dlqStore.count(), "DLQ should be empty")

	t.Logf("UC37: 3 bursts of %d, collected=%d, DLQ=%d",
		burstSize, got, dlqStore.count())
}

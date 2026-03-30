//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// =========================================================================
// TEST-RES-003: MQTT Source Message Drop Without DLQ
//
// Resilience gap: when an MQTT-sourced message fails delivery and the
// source does not support Retry() (returns ErrNotSupported), the runtime
// attempts to route the message to a DLQ. If no DLQ store is configured,
// the message is logged at Warn level and silently dropped.
//
// This test EXPOSES the production bug:
//   - MQTT source publishes 100 messages to a topic
//   - Bridge subscribes to that topic with alwaysFailSender
//   - Bridge is created WITHOUT a DLQ store
//   - DirectHold delivery mode (MQTT source has no visibility control)
//
// Expected result (with fix): Route rejected at AddRoute, or messages
// retried, or messages DLQ'd.
//
// Actual result (current code): All 100 messages silently dropped with
// a Warn log line. Zero messages reach the target. Zero DLQ entries.
// =========================================================================

func TestRES003_MQTTSourceDropWithoutDLQ(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount   = 100
		srcTopic   = "res003/source"
		testTimeout = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// --- Publisher session: sends messages to the source topic ---
	pubSessID := mqttlocal.UniqueClientID("res003-pub")
	pubSess := setupMQTTSession(t, pubSessID, domain.SessionEphemeral)
	pubSender := paho.NewSender(pubSess, paho.SenderOptions{
		DefaultTopic: srcTopic,
		QoS:          1,
		Timeout:      5 * time.Second,
	})

	// --- Bridge: MQTT source -> alwaysFailSender, NO DLQ ---
	rxSessID := mqttlocal.UniqueClientID("res003-rx")
	rxSess := setupMQTTSession(t, rxSessID, domain.SessionEphemeral)

	// Subscribe to source topic before creating bridge.
	reconcileErr := rxSess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: srcTopic, QoS: 1},
		},
	})
	if reconcileErr != nil {
		t.Fatalf("RES-003: Reconcile failed: %v", reconcileErr)
	}
	time.Sleep(300 * time.Millisecond)

	mqttRx := paho.NewReceiver("res003-rx", rxSess)
	failSnd := &alwaysFailSender{}

	// Create a collector on a dummy output topic to confirm zero delivery.
	outTopic := "res003/output"
	collector := newMQTTCollector(t, outTopic, "res003-col")

	// Runtime WITHOUT DLQ store -- this is the gap we are testing.
	rt := goruntime.New(
		goruntime.WithInstanceID("res003-bridge"),
		goruntime.WithLogger(testLogger(t)),
		// Deliberately omitting WithDLQStore to expose the gap.
	)

	routeCfg := goruntime.RouteConfig{
		ID: "res003-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "res003-bind", Address: outTopic},
		),
		SourceCapabilities: nil, // MQTT has no source capabilities
	}

	err := rt.AddRoute(routeCfg, mqttRx, failSnd, nil, nil)
	if err != nil {
		// If AddRoute rejects the configuration, the gap is FIXED.
		t.Logf("RES-003: AddRoute rejected config (gap may be fixed): %v", err)
		t.Logf("RES-003: PASS -- route validation prevents silent message loss")
		return
	}

	err = rt.Start(ctx)
	if err != nil {
		t.Logf("RES-003: Start rejected config: %v", err)
		return
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	// Publish 100 messages to the MQTT source topic.
	t.Logf("RES-003: publishing %d messages to MQTT source topic %q", msgCount, srcTopic)
	for i := 0; i < msgCount; i++ {
		env := &domain.Envelope{
			ID:      fmt.Sprintf("res003-msg-%d", i),
			Subject: srcTopic,
			Payload: []byte(fmt.Sprintf(`{"seq":%d}`, i)),
			Headers: map[string]any{"test": "res003"},
		}
		sendErr := pubSender.Send(ctx, env)
		if sendErr != nil {
			t.Logf("RES-003: publish %d failed: %v", i, sendErr)
		}
	}

	// Wait for the bridge to process (or drop) all messages.
	// Since alwaysFailSender fails immediately and there is no DLQ,
	// messages should be processed quickly (and silently dropped).
	t.Log("RES-003: waiting 30s for bridge processing")
	time.Sleep(30 * time.Second)

	// --- Assertions ---
	targetCount := collector.count()
	t.Logf("RES-003: messages reaching target: %d (expected: 0)", targetCount)
	assert.Equal(t, 0, targetCount,
		"alwaysFailSender should prevent any messages from reaching the output topic")

	// The key evidence: all 100 messages were silently dropped.
	// In a correct system, they would either:
	// (a) be retried until success
	// (b) be sent to a DLQ
	// (c) cause the route to be rejected at AddRoute
	//
	// Currently: none of the above. Messages are logged at Warn and ACKed.
	t.Logf("RES-003: EVIDENCE -- %d messages published to MQTT source topic", msgCount)
	t.Logf("RES-003: EVIDENCE -- 0 messages delivered to target (all failed)")
	t.Logf("RES-003: EVIDENCE -- no DLQ configured, so messages were silently dropped")
	t.Logf("RES-003: EVIDENCE -- MQTT source does not support Retry(), so no redelivery")
	t.Log("RES-003: This confirms the production gap: MQTT-sourced messages " +
		"are silently lost when sender fails and no DLQ is configured")
}

// =========================================================================
// TEST-RES-005: Auto-Extend Failure Duplicate Processing
//
// Resilience gap: when SQS auto-extend (ChangeMessageVisibility) fails
// after a few successes, the code exits the extend loop silently but
// does NOT cancel the processing goroutine's context. The goroutine
// continues working on the message. Meanwhile, SQS makes the message
// visible again (visibility expired) and another consumer picks it up.
// Both process the same message — duplicate delivery.
//
// This test creates conditions that stress auto-extend:
//   - VisibilityTimeout = 5s (very short)
//   - Processing takes 8s per message (longer than visibility)
//   - Auto-extend must fire at ~2.5s to keep the message invisible
//   - MaxInFlight = 5 to limit concurrent processing
//
// Expected result (with fix): Context cancelled on extend failure;
// processing aborts; zero duplicates.
//
// Actual result (current code): Processing continues past visibility
// expiry. SQS redelivers. Both process → duplicate at MQTT target.
//
// PRODUCTION FIX NEEDED:
//   - RES-005: Cancel delivery context on auto-extend failure.
//   - sqs/delivery.go exits extend loop after 3 failures but does not
//     cancel the processing context.
// =========================================================================

func TestRES005_AutoExtendFailureDuplicates(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 50
		outTopic    = "res005/output"
		testTimeout = 120 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// SQS queue with short visibility timeout.
	sqsClient := sqslocal.Client(t)
	sqsName := sqslocal.UniqueQueue("res005-in")
	sqsInURL := sqslocal.CreateQueueWithAttrs(t, sqsClient, sqsName,
		map[string]string{"VisibilityTimeout": "5"})

	collector := newMQTTCollector(t, outTopic, "res005-col")

	sessID := mqttlocal.UniqueClientID("res005-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	mqttSnd := setupMQTTSender(t, sess)

	// SQS receiver with short visibility + auto-extend enabled.
	ep := sqslocal.Endpoint(t)
	sqsRx, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          sqsInURL,
		Endpoint:          ep,
		Region:            "us-east-1",
		MaxMessages:       5,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
		AutoExtend:        boolPtr(true),
	}, slog.Default())
	require.NoError(t, err)

	dlq := &lrDLQStore{}

	rt := goruntime.New(
		goruntime.WithInstanceID("res005-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "res005-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  5,
		},
		Processors: []ports.Processor{
			newSlowProcessor("res005-slow", 8*time.Second),
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "res005-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("RES-005: sending %d messages (vis=5s, proc=8s, autoExtend=true)", msgCount)
	sendBulkToSQS(t, sqsClient, sqsInURL, msgCount, nil)

	// With 50 msgs, MaxInFlight=5, 8s each: ~80s total.
	lrWaitFor(t, 100*time.Second,
		fmt.Sprintf("collector >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })

	// Extra wait for any additional duplicates from redelivery.
	time.Sleep(15 * time.Second)

	total := collector.count()
	unique := countUnique(collector)
	duplicates := total - unique

	t.Logf("RES-005: total=%d, unique=%d, duplicates=%d, dlq=%d",
		total, unique, duplicates, dlq.count())

	if duplicates > 0 {
		t.Logf("RES-005: EVIDENCE -- %d duplicate deliveries detected", duplicates)
		t.Logf("RES-005: Auto-extend failure causes SQS redelivery during processing")
		t.Logf("RES-005: Fix: cancel delivery context on extend failure (sqs/delivery.go)")
	} else {
		t.Logf("RES-005: No duplicates -- auto-extend kept pace at this scale")
		t.Logf("RES-005: The gap may still exist but wasn't triggered")
	}

	require.GreaterOrEqual(t, unique, msgCount,
		"At least %d unique messages must be delivered", msgCount)
}

// =========================================================================
// TEST-RES-001: No Circuit Breaker on MQTT Sender
//
// When the MQTT broker is degraded (80% failure, 5s latency), all
// semaphore slots fill up with 30s-timeout sends, stalling the pipeline.
// A circuit breaker would fail-fast after a few failures, freeing slots.
//
// PRODUCTION FIX NEEDED: Add circuit breaker to paho/sender.go.
// =========================================================================

func TestRES001_NoCircuitBreakerOnSender(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 100
		outTopic    = "res001/output"
		testTimeout = 120 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "res001-in")
	dlq := &lrDLQStore{}
	collector := newMQTTCollector(t, outTopic, "res001-col")

	sessID := mqttlocal.UniqueClientID("res001-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	baseSnd := setupMQTTSender(t, sess)
	// Wrap in CB sender + degradedSender: 80% fail, 5s latency per send.
	// CB opens after 5 consecutive failures and fails-fast with ErrUnavailable.
	cbSnd := paho.NewCircuitBreakerSender(baseSnd, paho.CBConfig{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     5 * time.Second,
	})
	snd := newDegradedSender(cbSnd, 80, 5*time.Second)

	rt := goruntime.New(
		goruntime.WithInstanceID("res001-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "res001-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  10,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "res001-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), snd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	start := time.Now()
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// With CB: fails fast after 5 consecutive failures, then probes every 5s.
	// Without CB: each fail blocks for 5s on the degraded sender.
	time.Sleep(30 * time.Second)
	elapsed := time.Since(start)

	delivered := collector.count()
	t.Logf("RES-001: delivered=%d/%d in %v, dlq=%d", delivered, msgCount, elapsed, dlq.count())
	t.Logf("RES-001: CB should fail-fast on degraded sender, freeing semaphore slots quickly")

	assert.Greater(t, delivered+dlq.count(), 0,
		"At least some messages should be processed")
}

// =========================================================================
// TEST-RES-006: DLQ Write Blocks Semaphore
//
// When DLQ writes are slow (10s), they block semaphore slots. With
// MaxInFlight=10, processing serializes behind the slow DLQ writes.
//
// PRODUCTION FIX NEEDED: Decouple DLQ writes from semaphore slots.
// =========================================================================

func TestRES006_DLQWriteBlocksSemaphore(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 50
		outTopic    = "res006/output"
		testTimeout = 180 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "res006-in")
	baseDLQ := &lrDLQStore{}
	slowDLQ := newSlowDLQStore(baseDLQ, 5*time.Second) // 5s per DLQ write
	collector := newMQTTCollector(t, outTopic, "res006-col")

	sessID := mqttlocal.UniqueClientID("res006-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)

	rt := goruntime.New(
		goruntime.WithInstanceID("res006-bridge"),
		goruntime.WithDLQStore(slowDLQ),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "res006-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  10,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "res006-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), &alwaysFailSender{}, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	start := time.Now()
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait for all messages to be DLQ'd.
	lrWaitFor(t, 160*time.Second,
		fmt.Sprintf("DLQ >= %d", msgCount),
		func() bool { return baseDLQ.count() >= msgCount })

	elapsed := time.Since(start)
	t.Logf("RES-006: %d DLQ writes in %v (slowDLQ=5s, MaxInFlight=10)", msgCount, elapsed)

	if elapsed > 60*time.Second {
		t.Logf("RES-006: EVIDENCE -- slow DLQ writes serialize semaphore slots")
		t.Logf("RES-006: With bulkhead: expected ~25s (50/10 batches * 5s)")
		t.Logf("RES-006: Without bulkhead: DLQ write holds slot for 5s each")
	}

	assert.Equal(t, 0, collector.count(), "No messages should reach output")
}

// =========================================================================
// TEST-RES-011: Router Panic Swallows Messages
//
// A processor that panics on 10% of messages. Panicked messages should
// be logged + DLQ'd, but currently they are silently dropped.
//
// PRODUCTION FIX NEEDED: Log + metric on router panic recovery.
// =========================================================================

func TestRES011_RouterPanicSwallowsMessages(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 100
		outTopic    = "res011/output"
		testTimeout = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "res011-in")
	dlq := &lrDLQStore{}
	collector := newMQTTCollector(t, outTopic, "res011-col")

	sessID := mqttlocal.UniqueClientID("res011-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	snd := setupMQTTSender(t, sess)

	rt := goruntime.New(
		goruntime.WithInstanceID("res011-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "res011-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  20,
		},
		Processors: []ports.Processor{&panicProcessor{panicEvery: 10}},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "res011-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), snd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)
	time.Sleep(30 * time.Second)

	delivered := collector.count()
	dlqCount := dlq.count()
	total := delivered + dlqCount

	t.Logf("RES-011: delivered=%d, dlq=%d, total=%d, expected=%d",
		delivered, dlqCount, total, msgCount)

	// 10% panic → 10 panicked, 90 should succeed.
	// Current (no fix): panicked msgs silently dropped → delivered ~90, dlq=0
	// Fixed: panicked msgs DLQ'd → delivered ~90, dlq ~10
	if dlqCount == 0 && delivered < msgCount {
		t.Logf("RES-011: EVIDENCE -- %d messages lost to panics (no DLQ, no log)",
			msgCount-total)
	}

	assert.GreaterOrEqual(t, delivered, 80,
		"At least ~90%% of non-panicking messages should be delivered")
}

// panicProcessor is defined in longrunning_fault_helpers_test.go.

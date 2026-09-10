//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// RES-nnn resilience regressions (TEST-6).
//
// These were originally OBSERVATIONAL probes: each described an expected and a
// "broken" outcome, injected a fault, then logged which occurred while only
// softly asserting a weak floor — so a green run could be cited as evidence
// without ENFORCING the fixed behaviour. This file now injects each fault
// DETERMINISTICALLY and asserts exactly one expected result per regression, so
// a reintroduction of any gap turns the suite red instead of merely changing a
// log line. Every referenced gap is fixed at this revision; these lock it in.
// ═══════════════════════════════════════════════════════════════════════════

// TestRES003_MQTTSourceDropWithoutDLQ exposes the silent-drop hazard: an MQTT
// source (no Retry()) on a DirectHold route with NO DLQ store would drop a
// failed delivery with only a Warn log.
//
// With fix: the runtime REJECTS the route at admission (AddRoute/Start) rather
// than accepting a config that can silently lose messages — so the drop is
// impossible, not merely improbable.
func TestRES003_MQTTSourceDropWithoutDLQ(t *testing.T) {
	_ = withFreshInfra(t)
	const srcTopic = "res003/source"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rxSess := setupMQTTSession(t, mqttlocal.UniqueClientID("res003-rx"), connectivity.SessionEphemeral)
	require.NoError(t, rxSess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: srcTopic, QoS: 1}},
	}))
	waitSubReady(t, rxSess, 5*time.Second)
	mqttRx := paho.NewReceiver("res003-rx", rxSess)

	// Runtime WITHOUT a DLQ store, DirectHold, and an MQTT source that cannot
	// Retry(): the exact shape that would silently drop on send failure.
	rt := goruntime.New(goruntime.WithInstanceID("res003-bridge"), goruntime.WithLogger(testLogger(t)))
	routeCfg := goruntime.RouteConfig{
		ID:       "res003-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "res003-bind", Address: "res003/output"}),
	}
	err := rt.AddRoute(routeCfg, mqttRx, &alwaysFailSender{}, nil, nil)
	if err == nil {
		if err = rt.Start(ctx); err == nil {
			t.Cleanup(func() { _ = rt.Stop(context.Background()) })
		}
	}

	// STRICT (was: tolerate reject OR observe silent-drop): admission MUST
	// reject this config so no message can ever be silently dropped.
	require.Error(t, err,
		"runtime must REJECT DirectHold + no-DLQ + non-retryable MQTT source (would silently drop on failure)")
	require.ErrorContains(t, err, "silently dropped",
		"the rejection must name the silent-loss hazard so the operator can fix it (AllowRetryDrop / DLQ)")
}

// TestRES005_AutoExtendFailureDuplicates exposes duplicate delivery when SQS
// auto-extend cannot keep a long-processing message invisible.
//
// With fix: auto-extend keeps pace (VisibilityTimeout=5s, processing=8s), so
// SQS never redelivers and every message is delivered EXACTLY once.
func TestRES005_AutoExtendFailureDuplicates(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 50
		outTopic = "res005/output"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sqsClient := newSQSClient(t)
	sqsInURL := createSQSQueueWithAttrs(t, sqsClient, uniqueQueueName("res005-in"),
		map[string]string{"VisibilityTimeout": "5"})
	collector := newMQTTCollector(t, outTopic, "res005-col")

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("res005-sess"), connectivity.SessionEphemeral)
	sqsRx, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL: sqsInURL, Client: sqsClient, MaxMessages: 5, WaitTimeSeconds: 1,
		VisibilityTimeout: 5, AutoExtend: boolPtr(true),
	}, testLogger(t))
	require.NoError(t, err)

	rt := goruntime.New(
		goruntime.WithInstanceID("res005-bridge"),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "res005-route",
		// MaxInFlight=10 keeps wall time (~40s for 50 msgs × 8s / 10) well under
		// the 100s delivery budget on a loaded runner, while 8s processing still
		// exceeds the 5s visibility so auto-extend is exercised.
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 10},
		Processors:         []ports.Processor{newSlowProcessor("res005-slow", 8*time.Second)},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "res005-bind", Address: outTopic}),
		SourceCapabilities: directHoldCaps,
	}, sqsRx, setupMQTTSender(t, sess), sess, nil))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, sqsClient, sqsInURL, msgCount, nil)
	// Wait on the quantity the assertion below checks. Waiting on the raw
	// delivery count would return as soon as N deliveries had landed, and a
	// redelivery makes one of those a repeat of a message already seen while
	// another has not arrived — a correct at-least-once outcome the assertion
	// would then read as a lost message.
	lrWaitFor(t, 100*time.Second, fmt.Sprintf("unique >= %d", msgCount),
		func() bool { return countUnique(collector) >= msgCount })

	// Deterministic replacement for the old "sleep 15s and hope no more arrive":
	// wait until the arrival count STOPS changing, then assert exactly-once.
	settled := wait.StableFor(t, collector.count, 5*time.Second, 60*time.Second)
	require.Equal(t, msgCount, settled,
		"auto-extend must keep the message invisible: NO SQS redelivery, so no duplicate arrivals")
	require.Equal(t, msgCount, countUnique(collector),
		"every message delivered exactly once (no auto-extend duplicates)")
}

// TestRES001_NoCircuitBreakerOnSender injects a deterministic degraded sender
// (fixed-seed 80% failure + latency) behind a circuit breaker.
//
// With fix: the transient failures are retried via SQS redelivery and NEVER
// silently lost — the pipeline keeps making progress (not wedged) and no
// transient error is misclassified into the DLQ.
func TestRES001_NoCircuitBreakerOnSender(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 100
		outTopic = "res001/output"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "res001-in")
	dlq := &lrDLQStore{}
	collector := newMQTTCollector(t, outTopic, "res001-col")

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("res001-sess"), connectivity.SessionEphemeral)
	br := circuitbreaker.NewBreaker("res001-cb", circuitbreaker.Config{
		FailureThreshold: 5, SuccessThreshold: 2, ResetTimeout: 5 * time.Second,
		CountError: shared.IsRecoverableError,
	}, nil)
	// Deterministic (fixed-seed) 80% failure + 5s latency; the CB fails fast.
	snd := newDegradedSender(paho.NewCircuitBreakerSender(setupMQTTSender(t, sess), br), 80, 5*time.Second)

	rt := goruntime.New(
		goruntime.WithInstanceID("res001-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "res001-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 10},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "res001-bind", Address: outTopic}),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), snd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)
	// The CB fails fast rather than blocking a slot for the full latency, so the
	// pipeline drains SOME traffic within the bounded window.
	lrWaitFor(t, 60*time.Second, "pipeline makes progress under a degraded sender",
		func() bool { return collector.count() > 0 })

	// STRICT (was: delivered+dlq > 0): the transient degraded-sender failures are
	// recoverable, so NONE is ever misclassified into the DLQ, and the pipeline
	// is not wedged — a regression that wedged it (0 delivered) or misclassified
	// a transient failure (dlq > 0) turns this red.
	require.Zero(t, dlq.count(),
		"transient (recoverable) send failures must be retried, never DLQ'd")
	require.Positive(t, collector.count(),
		"the circuit breaker must keep the pipeline making progress under a degraded sender")
}

// TestRES006_DLQWriteBlocksSemaphore drives permanent failures through a slow
// (5s) DLQ store with MaxInFlight=10.
//
// With fix: DLQ writes are bulkheaded off the delivery slots, so every
// permanently-failed message reaches the DLQ and none leaks to the output.
func TestRES006_DLQWriteBlocksSemaphore(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 50
		outTopic = "res006/output"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "res006-in")
	baseDLQ := &lrDLQStore{}
	collector := newMQTTCollector(t, outTopic, "res006-col")
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("res006-sess"), connectivity.SessionExclusive)

	rt := goruntime.New(
		goruntime.WithInstanceID("res006-bridge"),
		goruntime.WithDLQStore(newSlowDLQStore(baseDLQ, 5*time.Second)),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "res006-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 10},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "res006-bind", Address: outTopic}),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), &permanentFailSender{}, sess, nil))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)
	lrWaitFor(t, 160*time.Second, fmt.Sprintf("DLQ >= %d", msgCount),
		func() bool { return baseDLQ.count() >= msgCount })

	// STRICT (was: only collector==0, with an observational elapsed>60s log):
	// every permanently-failed message reaches the DLQ, and NONE reaches output.
	require.Equal(t, msgCount, baseDLQ.count(),
		"every permanently-failed message must reach the DLQ despite slow writes")
	require.Zero(t, collector.count(), "no permanently-failed message may reach the output")
}

// TestRES011_RouterPanicSwallowsMessages injects a processor that panics on
// every 10th message.
//
// With fix: a panicked message is recovered and routed to the DLQ — it is NOT
// silently swallowed, so delivered + DLQ accounts for every message.
func TestRES011_RouterPanicSwallowsMessages(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount   = 100
		panicEvery = 10
		outTopic   = "res011/output"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "res011-in")
	dlq := &lrDLQStore{}
	collector := newMQTTCollector(t, outTopic, "res011-col")
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("res011-sess"), connectivity.SessionEphemeral)

	rt := goruntime.New(
		goruntime.WithInstanceID("res011-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "res011-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 20},
		Processors:         []ports.Processor{&panicProcessor{panicEvery: panicEvery}},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "res011-bind", Address: outTopic}),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), setupMQTTSender(t, sess), sess, nil))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)
	lrWaitFor(t, 40*time.Second, fmt.Sprintf("delivered+dlq == %d", msgCount),
		func() bool { return collector.count()+dlq.count() >= msgCount })
	settled := wait.StableFor(t, func() int { return collector.count() + dlq.count() }, 3*time.Second, 40*time.Second)

	// STRICT (was: delivered >= 80, with an observational "lost" log): every
	// message is accounted for — the panicked ones (every 10th) are DLQ'd, not
	// silently swallowed.
	require.Equal(t, msgCount, settled, "no message may be silently swallowed by a processor panic")
	require.Equal(t, msgCount/panicEvery, dlq.count(),
		"exactly the panicked messages (every %dth) must be routed to the DLQ", panicEvery)
	require.Equal(t, msgCount-msgCount/panicEvery, collector.count(),
		"all non-panicking messages must be delivered")
}

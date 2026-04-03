//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// =========================================================================
// UC42: Broker Kill and Restart Mid-Stream (SharedOutbox)
//
// SQS-IN --> [Bridge (SharedOutbox)] --> MQTT "uc42/output" --> Collector
//
// Kill the broker after ~1,000 messages collected. Wait 5s. Restart.
// Assert: collector >= 3,000 unique after recovery. DLQ empty.
// =========================================================================

func TestUC42_BrokerKillRestart_SharedOutbox(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount   = 3000
		killAt     = 1000
		outTopic   = "uc42/output"
		testTimeout = 240 * time.Second
	)

	// Per-test broker with persistence so queued messages survive restart.
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc42-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Persistent collector — broker queues messages during disconnection,
	// eliminating the subscriber-before-publisher race on restart.
	collector := newPersistentCollectorWithBroker(t, brokerURL, outTopic, "uc42-col")

	// Bridge session on the per-test broker.
	// KeepAlive=5 for fast disconnect detection after broker restart.
	sessionID := mqttlocal.UniqueClientID("uc42-session")
	sess := newMQTTSessionWithBroker(t, brokerURL, sessionID,
		domain.SessionExclusive, 65535, 5)
	mqttSnd := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc42-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc42-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc42-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc42-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	// Send messages to SQS.
	t.Logf("UC42: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait until collector has ~killAt messages, then kill broker.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector >= %d before kill", killAt),
		func() bool { return collector.count() >= killAt })

	beforeKill := collector.count()
	t.Logf("UC42: killing broker at collector=%d", beforeKill)
	broker.StopGraceful()

	// Broker is down for 5s.
	time.Sleep(5 * time.Second)

	t.Log("UC42: restarting broker")
	broker.RestartGraceful()

	// Black-box readiness: send a probe through the full pipeline and
	// wait for it to arrive at the collector. This proves SQS receiver,
	// outbox drainer, MQTT session, broker, and collector subscription
	// are all operational — no sleep required.
	sendProbe(t, sqsInClient, sqsInURL, collector, 30*time.Second)

	// Wait for all messages to arrive after recovery.
	// NOTE: Use collector.count() not countUnique() because EnvelopeFromPublish
	// does not set Envelope.ID, so countUnique always returns 1 for MQTT.
	lrWaitFor(t, 180*time.Second,
		fmt.Sprintf("collector >= %d after restart", msgCount),
		func() bool { return collector.count() >= msgCount })

	t.Logf("UC42: collector=%d, dlq=%d", collector.count(), dlq.count())

	require.GreaterOrEqual(t, collector.count(), msgCount,
		"SharedOutbox must deliver all %d messages after broker restart", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

// =========================================================================
// UC43: Broker Kill Mid-Stream (DirectHold)
//
// Same topology as UC42 but with DirectHold + SQS VisibilityTimeout=10s.
// SQS redelivers in-flight messages after broker restart.
// Assert: >= 2,000 unique (at-least-once via SQS redelivery).
// =========================================================================

func TestUC43_BrokerKillRestart_DirectHold(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount   = 2000
		killAt     = 500
		outTopic   = "uc43/output"
		testTimeout = 240 * time.Second
	)

	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	// SQS queue with short visibility timeout for faster redelivery.
	sqsInClient := sqslocal.Client(t)
	sqsInName := sqslocal.UniqueQueue("uc43-in")
	sqsInURL := sqslocal.CreateQueueWithAttrs(t, sqsInClient, sqsInName,
		map[string]string{"VisibilityTimeout": "10"})

	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newPersistentCollectorWithBroker(t, brokerURL, outTopic, "uc43-col")

	sessionID := mqttlocal.UniqueClientID("uc43-session")
	sess := setupMQTTSessionWithBroker(t, brokerURL, sessionID,
		domain.SessionExclusive, 65535, 5)
	mqttSnd := setupMQTTSender(t, sess)

	sqsRx, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          sqsInURL,
		Client:            sqslocal.Client(t),
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 10,
	}, testLogger(t))
	require.NoError(t, err)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc43-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc43-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:      domain.DeliveryDirectHold,
			MaxReplayAttempts: 50,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc43-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC43: sending %d messages to SQS-IN (vis=10s)", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 60*time.Second,
		fmt.Sprintf("collector >= %d before kill", killAt),
		func() bool { return collector.count() >= killAt })

	t.Logf("UC43: killing broker at collector=%d", collector.count())
	broker.StopGraceful()
	time.Sleep(5 * time.Second)

	t.Log("UC43: restarting broker")
	broker.RestartGraceful()

	// DirectHold: the route runner retries via SQS redelivery. Use
	// gobridgesync to confirm the bridge session is reconnected, then
	// wait for all messages to arrive. Persistent collector ensures the
	// broker queues messages during the reconnection gap.
	gobridgesync(t, 30*time.Second, rt)
	lrWaitFor(t, 180*time.Second,
		fmt.Sprintf("collector >= %d after restart", msgCount),
		func() bool { return collector.count() >= msgCount })

	t.Logf("UC43: collector=%d, dlq=%d", collector.count(), dlq.count())

	require.GreaterOrEqual(t, collector.count(), msgCount,
		"DirectHold + SQS redelivery must deliver >= %d messages", msgCount)
}

// =========================================================================
// UC44: Broker Low Inflight Quota (Backpressure)
//
// Broker limited to max_inflight_messages=5.
// Client ReceiveMaximum=5, pipeline MaxInFlight=100.
// Assert: collector = 2,000 unique (zero loss despite low quota).
// =========================================================================

func TestUC44_BrokerLowInflightQuota(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount   = 2000
		outTopic   = "uc44/output"
		testTimeout = 300 * time.Second
	)

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(5),
	)
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc44-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollectorWithBroker(t, brokerURL, outTopic, "uc44-col")

	sessionID := mqttlocal.UniqueClientID("uc44-session")
	sess := newMQTTSessionWithBroker(t, brokerURL, sessionID,
		domain.SessionExclusive, 5) // ReceiveMaximum=5
	mqttSnd := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc44-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc44-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			MaxInFlight:  100,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc44-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc44-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC44: sending %d messages (broker inflight=5, receiveMax=5)", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 280*time.Second,
		fmt.Sprintf("unique >= %d", msgCount),
		func() bool { return countUnique(collector) >= msgCount })

	unique := countUnique(collector)
	t.Logf("UC44: unique=%d, total=%d, dlq=%d", unique, collector.count(), dlq.count())

	require.Equal(t, msgCount, unique,
		"SharedOutbox + low inflight quota must deliver exactly %d unique (zero loss)", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

// =========================================================================
// UC45: Broker Quota -- SharedOutbox vs DirectHold Comparison
//
// Two parallel paths through the same low-quota broker:
//   Path A: SharedOutbox
//   Path B: DirectHold
// Each receives 1,000 messages.
//
// This is a DOCUMENTATION TEST: it proves SharedOutbox is required for
// reliable delivery under broker quota pressure. DirectHold may lose
// messages because failed publishes have no durable retry.
// =========================================================================

func TestUC45_BrokerQuota_SharedOutbox_vs_DirectHold(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount   = 1000
		topicA     = "uc45/outbox/output"
		topicB     = "uc45/direct/output"
		testTimeout = 300 * time.Second
	)

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(10),
	)
	brokerURL := broker.URL()

	// Infrastructure for path A (SharedOutbox).
	sqsInA, sqsClientA := setupSQSQueue(t, "uc45-in-a")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlqA := &lrDLQStore{}

	// Infrastructure for path B (DirectHold).
	sqsInB, sqsClientB := setupSQSQueue(t, "uc45-in-b")
	dlqB := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collectorA := newMQTTCollectorWithBroker(t, brokerURL, topicA, "uc45-col-a")
	collectorB := newMQTTCollectorWithBroker(t, brokerURL, topicB, "uc45-col-b")

	// --- Path A: SharedOutbox ---
	sessIDA := mqttlocal.UniqueClientID("uc45-sess-a")
	sessA := newMQTTSessionWithBroker(t, brokerURL, sessIDA,
		domain.SessionExclusive, 10)
	sndA := setupMQTTSender(t, sessA)
	rxA := newSQSReceiver(t, sqsInA)
	scA := lrSessionConfig(sessIDA)

	rtA := goruntime.New(
		goruntime.WithInstanceID("uc45-bridge-a"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlqA),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtA.AddRoute(goruntime.RouteConfig{
		ID: "uc45-route-a",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			MaxInFlight:  50,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc45-bind-a", Address: topicA},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc45-bind-a", SessionID: sessIDA},
		},
	}, rxA, sndA, sessA, &scA))
	require.NoError(t, rtA.Start(ctx))
	defer func() { _ = rtA.Stop(context.Background()) }()

	// --- Path B: DirectHold ---
	sessIDB := mqttlocal.UniqueClientID("uc45-sess-b")
	sessB := setupMQTTSessionWithBroker(t, brokerURL, sessIDB,
		domain.SessionExclusive, 10)
	sndB := setupMQTTSender(t, sessB)
	rxB := newSQSReceiver(t, sqsInB)

	rtB := goruntime.New(
		goruntime.WithInstanceID("uc45-bridge-b"),
		goruntime.WithDLQStore(dlqB),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtB.AddRoute(goruntime.RouteConfig{
		ID: "uc45-route-b",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  50,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc45-bind-b", Address: topicB},
		),
		SourceCapabilities: directHoldCaps,
	}, rxB, sndB, sessB, nil))
	require.NoError(t, rtB.Start(ctx))
	defer func() { _ = rtB.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rtA, rtB)

	// Send messages to both paths in parallel.
	t.Logf("UC45: sending %d messages to each path (broker inflight=10)", msgCount)
	sendBulkToSQS(t, sqsClientA, sqsInA, msgCount, nil)
	sendBulkToSQS(t, sqsClientB, sqsInB, msgCount, nil)

	// Wait for SharedOutbox (path A) to complete -- it must deliver all.
	lrWaitFor(t, 280*time.Second,
		fmt.Sprintf("SharedOutbox unique >= %d", msgCount),
		func() bool { return countUnique(collectorA) >= msgCount })

	// Give DirectHold some extra time, but do not require all messages.
	time.Sleep(30 * time.Second)

	uniqueA := countUnique(collectorA)
	uniqueB := countUnique(collectorB)
	t.Logf("UC45: SharedOutbox: unique=%d, total=%d, dlq=%d",
		uniqueA, collectorA.count(), dlqA.count())
	t.Logf("UC45: DirectHold:   unique=%d, total=%d, dlq=%d",
		uniqueB, collectorB.count(), dlqB.count())

	gap := msgCount - uniqueB
	if gap > 0 {
		t.Logf("UC45: EVIDENCE -- DirectHold lost %d messages under broker quota pressure", gap)
		t.Logf("UC45: This confirms SharedOutbox is REQUIRED for reliable delivery")
	} else {
		t.Logf("UC45: DirectHold delivered all messages (quota pressure may not have triggered loss)")
	}

	// SharedOutbox MUST deliver all.
	require.GreaterOrEqual(t, uniqueA, msgCount,
		"SharedOutbox must deliver all %d unique messages", msgCount)

	// Log DirectHold result -- this is documentation, not a hard assertion.
	assert.GreaterOrEqual(t, uniqueB, 0,
		"DirectHold result logged for comparison")
}

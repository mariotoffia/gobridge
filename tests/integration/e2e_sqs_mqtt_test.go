package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// S1-S8: Basic E2E scenarios with real SQS, MQTT, and DynamoDB adapters
// ═══════════════════════════════════════════════════════════════════════════

// validates basic SQS to MQTT delivery using direct_hold.
//
// ElasticMQ to Mosquitto; the subscriber receives the same payload as sent to the queue.
func TestE2E_S1_SQSToMQTT_DirectHold(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "s1")
	sessionID := mqttlocal.UniqueClientID("s1-mqtt")
	topic := "e2e/s1/data"

	collector := newMQTTCollector(t, topic, "s1-sub")

	sess := setupMQTTSession(t, sessionID, domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsReceiver := newSQSReceiver(t, queueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("s1-bridge"),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)

	cfg := goruntime.RouteConfig{
		ID: "s1-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bind-1", Address: topic},
		),
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}

	if err := rt.AddRoute(cfg, sqsReceiver, mqttSender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"temp":22.5}`, map[string]string{"device": "sensor-1"})

	e2eWaitFor(t, 10*time.Second, "MQTT message received", func() bool {
		return collector.count() >= 1
	})

	msgs := collector.getMessages()
	if string(msgs[0].Payload) != `{"temp":22.5}` {
		t.Errorf("payload = %q, want %q", string(msgs[0].Payload), `{"temp":22.5}`)
	}
}

// validates SQS to MQTT via shared_outbox using DynamoDB lease and outbox stores.
func TestE2E_S2_SQSToMQTT_SharedOutbox(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "s2")
	sessionID := mqttlocal.UniqueClientID("s2-mqtt")
	topic := "e2e/s2/data"

	collector := newMQTTCollector(t, topic, "s2-sub")

	sess := setupMQTTSession(t, sessionID, domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsReceiver := newSQSReceiver(t, queueURL)
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	sessCfg := e2eFastSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("s2-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)

	cfg := goruntime.RouteConfig{
		ID: "s2-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bind-1", Address: topic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "bind-1", SessionID: sessionID},
		},
	}

	if err := rt.AddRoute(cfg, sqsReceiver, mqttSender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"order":"abc"}`, nil)

	e2eWaitFor(t, 15*time.Second, "MQTT message via outbox", func() bool {
		return collector.count() >= 1
	})

	msgs := collector.getMessages()
	if string(msgs[0].Payload) != `{"order":"abc"}` {
		t.Errorf("payload = %q, want %q", string(msgs[0].Payload), `{"order":"abc"}`)
	}
}

// validates MQTT to SQS delivery using direct_hold.
func TestE2E_S3_MQTTToSQS_DirectHold(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "s3")
	sessionID := mqttlocal.UniqueClientID("s3-mqtt")
	topic := "e2e/s3/ingress"

	sess := setupMQTTSession(t, sessionID, domain.SessionEphemeral)

	if err := sess.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	mqttReceiver := paho.NewReceiver("s3-rx", sess)
	sqsSender := newSQSSender(t, queueURL)
	dlq := &e2eDLQStore{}

	rt := goruntime.New(
		goruntime.WithInstanceID("s3-bridge"),
		goruntime.WithDLQStore(dlq),
	)

	cfg := goruntime.RouteConfig{
		ID: "s3-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "sqs-bind", Address: queueURL},
		),
		SourceCapabilities: directHoldCaps,
	}

	if err := rt.AddRoute(cfg, mqttReceiver, sqsSender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("s3-pub"), domain.SessionEphemeral)
	pubSender := paho.NewSender(pubSess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	env := &domain.Envelope{
		ID:      "s3-msg-1",
		Subject: topic,
		Payload: []byte(`{"event":"created"}`),
	}
	if err := pubSender.Send(context.Background(), env); err != nil {
		t.Fatalf("MQTT Send: %v", err)
	}

	bodies := pollSQS(t, sqsClient, queueURL, 1, 10*time.Second)
	if len(bodies) < 1 {
		t.Fatal("expected at least 1 SQS message")
	}
	if bodies[0] != `{"event":"created"}` {
		t.Errorf("SQS body = %q, want %q", bodies[0], `{"event":"created"}`)
	}
}

// validates recovery after bridge crash: shared-outbox records drain to MQTT once a new instance runs with a fast drain.
func TestE2E_S4_SQSToMQTT_BridgeCrashAndRestart(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "s4")
	sessionID := mqttlocal.UniqueClientID("s4-mqtt")
	topic := "e2e/s4/data"

	collector := newMQTTCollector(t, topic, "s4-sub")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	// Instance A with very long drain to prevent drain before crash.
	sessA := setupMQTTSession(t, sessionID+"-a", domain.SessionEphemeral)
	mqttSenderA := setupMQTTSender(t, sessA)
	sqsReceiverA := newSQSReceiver(t, queueURL)

	sessCfgA := e2eFastSessionConfig(sessionID)
	sessCfgA.DrainStrategy = domain.NewFixedPoll(30 * time.Second)

	rtA := goruntime.New(
		goruntime.WithInstanceID("s4-bridge-A"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)

	cfgA := goruntime.RouteConfig{
		ID: "s4-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bind-1", Address: topic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "bind-1", SessionID: sessionID},
		},
	}

	_ = rtA.AddRoute(cfgA, sqsReceiverA, mqttSenderA, sessA, &sessCfgA)

	ctxA, cancelA := context.WithCancel(context.Background())
	_ = rtA.Start(ctxA)

	sendToSQS(t, sqsClient, queueURL, `{"crash":"test"}`, nil)

	time.Sleep(3 * time.Second)

	cancelA()
	_ = rtA.Stop(context.Background())

	// Restart with new instance that will drain quickly.
	sessB := setupMQTTSession(t, sessionID+"-b", domain.SessionEphemeral)
	mqttSenderB := setupMQTTSender(t, sessB)
	sqsReceiverB := newSQSReceiver(t, queueURL)

	sessCfgB := e2eFastSessionConfig(sessionID)

	rtB := goruntime.New(
		goruntime.WithInstanceID("s4-bridge-B"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)

	cfgB := goruntime.RouteConfig{
		ID: "s4-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bind-1", Address: topic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "bind-1", SessionID: sessionID},
		},
	}

	_ = rtB.AddRoute(cfgB, sqsReceiverB, mqttSenderB, sessB, &sessCfgB)

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	_ = rtB.Start(ctxB)
	defer func() { _ = rtB.Stop(context.Background()) }()

	e2eWaitFor(t, 20*time.Second, "MQTT after restart", func() bool {
		return collector.count() >= 1
	})

	t.Log("S4: Bridge crash-restart delivered message to MQTT")
}

// validates secondary bridge takeover: instance A persists without MQTT drain; instance B acquires the lease and delivers to MQTT.
func TestE2E_S5_SQSToMQTT_SecondaryBridgeTakeover(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "s5")
	sessionID := mqttlocal.UniqueClientID("s5-mqtt")
	topic := "e2e/s5/data"

	collector := newMQTTCollector(t, topic, "s5-sub")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	// Instance A: ingress only (no MQTT session for drain).
	sqsReceiverA := newSQSReceiver(t, queueURL)

	rtA := goruntime.New(
		goruntime.WithInstanceID("s5-bridge-A"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)

	cfgA := goruntime.RouteConfig{
		ID: "s5-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bind-1", Address: topic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "bind-1", SessionID: sessionID},
		},
	}

	senderA := newSQSSender(t, queueURL)
	_ = rtA.AddRoute(cfgA, sqsReceiverA, senderA, nil, nil)

	ctxA, cancelA := context.WithCancel(context.Background())
	_ = rtA.Start(ctxA)

	sendToSQS(t, sqsClient, queueURL, `{"takeover":"test"}`, nil)

	time.Sleep(3 * time.Second)

	cancelA()
	_ = rtA.Stop(context.Background())

	// Instance B: owns the MQTT session and drains.
	sessB := setupMQTTSession(t, sessionID+"-b", domain.SessionEphemeral)
	mqttSenderB := setupMQTTSender(t, sessB)
	sqsReceiverB := newSQSReceiver(t, queueURL)

	sessCfgB := e2eFastSessionConfig(sessionID)

	rtB := goruntime.New(
		goruntime.WithInstanceID("s5-bridge-B"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)

	cfgB := goruntime.RouteConfig{
		ID: "s5-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bind-1", Address: topic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "bind-1", SessionID: sessionID},
		},
	}

	_ = rtB.AddRoute(cfgB, sqsReceiverB, mqttSenderB, sessB, &sessCfgB)

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	_ = rtB.Start(ctxB)
	defer func() { _ = rtB.Stop(context.Background()) }()

	e2eWaitFor(t, 20*time.Second, "MQTT via secondary bridge", func() bool {
		return collector.count() >= 1
	})

	t.Log("S5: Secondary bridge took over and delivered to MQTT")
}

// validates end-to-end round-trip from SQS queue A through MQTT to SQS queue B.
func TestE2E_S6_SQSToMQTT_RoundTrip(t *testing.T) {
	queueA, sqsClientA := setupSQSQueue(t, "s6-a")
	queueB, sqsClientB := setupSQSQueue(t, "s6-b")
	sessionID := mqttlocal.UniqueClientID("s6-mqtt")
	topic := "e2e/s6/roundtrip"

	sess := setupMQTTSession(t, sessionID, domain.SessionEphemeral)

	if err := sess.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	mqttSender := setupMQTTSender(t, sess)
	mqttReceiver := paho.NewReceiver("s6-rx", sess)
	sqsReceiverA := newSQSReceiver(t, queueA)
	sqsSenderB := newSQSSender(t, queueB)
	dlq := &e2eDLQStore{}

	rt := goruntime.New(
		goruntime.WithInstanceID("s6-bridge"),
		goruntime.WithDLQStore(dlq),
	)

	// Route 1: SQS-A -> MQTT topic
	route1 := goruntime.RouteConfig{
		ID: "s6-route-sqs-to-mqtt",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-bind", Address: topic},
		),
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}
	if err := rt.AddRoute(route1, sqsReceiverA, mqttSender, nil, nil); err != nil {
		t.Fatalf("AddRoute route1: %v", err)
	}

	// Route 2: MQTT topic -> SQS-B
	route2 := goruntime.RouteConfig{
		ID: "s6-route-mqtt-to-sqs",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "sqs-bind", Address: queueB},
		),
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}
	if err := rt.AddRoute(route2, mqttReceiver, sqsSenderB, nil, nil); err != nil {
		t.Fatalf("AddRoute route2: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClientA, queueA, `{"roundtrip":"s6"}`, nil)

	bodies := pollSQS(t, sqsClientB, queueB, 1, 15*time.Second)
	if len(bodies) < 1 {
		t.Fatal("expected message in SQS queue B")
	}
	_ = sqsClientB // suppress unused
	t.Logf("S6: Round-trip complete, body = %s", bodies[0])
}

// validates twenty messages flow through shared_outbox to MQTT without loss.
func TestE2E_S7_SQSToMQTT_MultipleMessages(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "s7")
	sessionID := mqttlocal.UniqueClientID("s7-mqtt")
	topic := "e2e/s7/data"

	collector := newMQTTCollector(t, topic, "s7-sub")

	sess := setupMQTTSession(t, sessionID, domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsReceiver := newSQSReceiver(t, queueURL)
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	sessCfg := e2eFastSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("s7-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)

	cfg := goruntime.RouteConfig{
		ID: "s7-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bind-1", Address: topic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "bind-1", SessionID: sessionID},
		},
	}

	_ = rt.AddRoute(cfg, sqsReceiver, mqttSender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	const msgCount = 20
	for i := 0; i < msgCount; i++ {
		body := fmt.Sprintf(`{"seq":%d}`, i)
		sendToSQS(t, sqsClient, queueURL, body, nil)
	}

	e2eWaitFor(t, 30*time.Second, "all 20 MQTT messages", func() bool {
		return collector.count() >= msgCount
	})

	t.Logf("S7: Received %d messages on MQTT", collector.count())
}

// validates route processors enrich envelopes before MQTT delivery under direct_hold.
func TestE2E_S8_SQSToMQTT_ProcessorChain(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "s8")
	sessionID := mqttlocal.UniqueClientID("s8-mqtt")
	topic := "e2e/s8/data"

	collector := newMQTTCollector(t, topic, "s8-sub")

	sess := setupMQTTSession(t, sessionID, domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsReceiver := newSQSReceiver(t, queueURL)
	dlq := &e2eDLQStore{}

	enricher := &testProcessor{
		name: "enricher",
		fn: func(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
			env.Headers["enriched-by"] = "s8-test"
			return next(ctx, env)
		},
	}

	rt := goruntime.New(
		goruntime.WithInstanceID("s8-bridge"),
		goruntime.WithDLQStore(dlq),
	)

	cfg := goruntime.RouteConfig{
		ID: "s8-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Processors: []ports.Processor{enricher},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bind-1", Address: topic},
		),
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}

	_ = rt.AddRoute(cfg, sqsReceiver, mqttSender, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"data":"enrich-me"}`, nil)

	e2eWaitFor(t, 10*time.Second, "enriched MQTT message", func() bool {
		return collector.count() >= 1
	})

	msgs := collector.getMessages()
	if msgs[0].Headers["enriched-by"] != "s8-test" {
		t.Errorf("expected enriched-by header, got headers: %v", msgs[0].Headers)
	}
}

// testProcessor is a simple ports.Processor for tests.
type testProcessor struct {
	name string
	fn   func(context.Context, *domain.Envelope, ports.ProcessorFunc) error
}

func (p *testProcessor) Name() string { return p.name }
func (p *testProcessor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	return p.fn(ctx, env, next)
}

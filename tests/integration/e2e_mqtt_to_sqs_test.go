package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// verifies single-topic MQTT ingress delivers all published messages to one SQS queue using direct_hold.
func TestE2E_MQTTToSQS_SingleTopic(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "m1")
	topic := "sensors/temp"

	bridgeSess := setupMQTTSession(t, mqttlocal.UniqueClientID("m1-bridge"), connectivity.SessionEphemeral)
	if err := bridgeSess.Reconcile(context.Background(), connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubReady(t, bridgeSess)

	mqttRx := paho.NewReceiver("m1-rx", bridgeSess)
	sqsTx := newSQSSender(t, queueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("m1-bridge"),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)
	cfg := goruntime.RouteConfig{
		ID:                 "m1-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "sqs-bind", Address: queueURL}),
		SourceCapabilities: directHoldCaps,
	}
	if err := rt.AddRoute(cfg, mqttRx, sqsTx, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("m1-pub"), connectivity.SessionEphemeral)
	pubTx := paho.NewSender(pubSess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	for i := 0; i < 5; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("m1-msg-%d", i),
			Subject: topic,
			Payload: []byte(fmt.Sprintf(`{"temp":%d}`, 20+i)),
		})
		if err := pubTx.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: topic}); err != nil {
			t.Fatalf("MQTT Send %d: %v", i, err)
		}
	}

	bodies := pollSQS(t, sqsClient, queueURL, 5, 10*time.Second)
	if len(bodies) < 5 {
		t.Fatalf("expected 5 SQS messages, got %d", len(bodies))
	}
}

// verifies three MQTT topics merge into one SQS queue when three routes share one session.
//
// The session router fans out each message to every receiver, so three publishes yield at least three SQS arrivals.
func TestE2E_MQTTToSQS_MultiTopicMerge(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "m2")
	topics := []string{"devices/temp", "devices/humid", "devices/press"}

	bridgeSess := setupMQTTSession(t, mqttlocal.UniqueClientID("m2-bridge"), connectivity.SessionEphemeral)
	subs := make([]connectivity.SubscriptionPlan, len(topics))
	for i, tp := range topics {
		subs[i] = connectivity.SubscriptionPlan{Topic: tp, QoS: 1}
	}
	if err := bridgeSess.Reconcile(context.Background(), connectivity.SessionPlan{Subscriptions: subs}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubReady(t, bridgeSess)

	sqsTx := newSQSSender(t, queueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("m2-bridge"),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)
	for i := range topics {
		rx := paho.NewReceiver(fmt.Sprintf("m2-rx-%d", i), bridgeSess)
		cfg := goruntime.RouteConfig{
			ID:                 fmt.Sprintf("m2-route-%d", i),
			Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
			Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "sqs-bind", Address: queueURL}),
			SourceCapabilities: directHoldCaps,
		}
		if err := rt.AddRoute(cfg, rx, sqsTx, nil, nil); err != nil {
			t.Fatalf("AddRoute %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("m2-pub"), connectivity.SessionEphemeral)
	pubTx := paho.NewSender(pubSess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	for i, tp := range topics {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("m2-msg-%d", i),
			Subject: tp,
			Payload: []byte(fmt.Sprintf(`{"topic":"%s"}`, tp)),
		})
		if err := pubTx.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: tp}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	bodies := pollSQS(t, sqsClient, queueURL, 3, 10*time.Second)
	if len(bodies) < 3 {
		t.Fatalf("expected at least 3 SQS messages, got %d", len(bodies))
	}
}

// verifies two MQTT topics on separate sessions deliver to distinct SQS queues without cross-routing.
func TestE2E_MQTTToSQS_HeaderBasedRouting(t *testing.T) {
	ordersQueue, ordersClient := setupSQSQueue(t, "m3-orders")
	alertsQueue, alertsClient := setupSQSQueue(t, "m3-alerts")
	ordersTopic := "events/orders"
	alertsTopic := "events/alerts"

	reconcileTopic := func(clientPrefix, topic string) *paho.Session {
		s := setupMQTTSession(t, mqttlocal.UniqueClientID(clientPrefix), connectivity.SessionEphemeral)
		if err := s.Reconcile(context.Background(), connectivity.SessionPlan{
			Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
		}); err != nil {
			t.Fatalf("Reconcile %s: %v", topic, err)
		}
		return s
	}
	ordersSess := reconcileTopic("m3-orders", ordersTopic)
	alertsSess := reconcileTopic("m3-alerts", alertsTopic)
	waitSubReady(t, ordersSess)
	waitSubReady(t, alertsSess)

	rt := goruntime.New(
		goruntime.WithInstanceID("m3-bridge"),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)

	ordersCfg := goruntime.RouteConfig{
		ID:                 "m3-orders",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "orders-bind", Address: ordersQueue}),
		SourceCapabilities: directHoldCaps,
	}
	if err := rt.AddRoute(ordersCfg, paho.NewReceiver("m3-orders-rx", ordersSess), newSQSSender(t, ordersQueue), nil, nil); err != nil {
		t.Fatalf("AddRoute orders: %v", err)
	}

	alertsCfg := goruntime.RouteConfig{
		ID:                 "m3-alerts",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "alerts-bind", Address: alertsQueue}),
		SourceCapabilities: directHoldCaps,
	}
	if err := rt.AddRoute(alertsCfg, paho.NewReceiver("m3-alerts-rx", alertsSess), newSQSSender(t, alertsQueue), nil, nil); err != nil {
		t.Fatalf("AddRoute alerts: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("m3-pub"), connectivity.SessionEphemeral)
	pubTx := paho.NewSender(pubSess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	_ = pubTx.Send(context.Background(), ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m3-order", Subject: ordersTopic, Payload: []byte(`{"order":"123"}`)}), Address: ordersTopic})
	_ = pubTx.Send(context.Background(), ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m3-alert", Subject: alertsTopic, Payload: []byte(`{"alert":"fire"}`)}), Address: alertsTopic})

	ordersBodies := pollSQS(t, ordersClient, ordersQueue, 1, 10*time.Second)
	alertsBodies := pollSQS(t, alertsClient, alertsQueue, 1, 10*time.Second)
	if len(ordersBodies) < 1 || ordersBodies[0] != `{"order":"123"}` {
		t.Errorf("orders queue: expected order message, got %v", ordersBodies)
	}
	if len(alertsBodies) < 1 || alertsBodies[0] != `{"alert":"fire"}` {
		t.Errorf("alerts queue: expected alert message, got %v", alertsBodies)
	}
}

// verifies shared-outbox failover from SQS through MQTT to a second SQS queue.
//
// Bridge A persists to the outbox and stops before drain; bridge B drains to MQTT and a second route forwards to SQS-B.
func TestE2E_MQTTToSQS_RoundTripWithFailover(t *testing.T) {
	queueA, sqsClientA := setupSQSQueue(t, "m4-a")
	queueB, sqsClientB := setupSQSQueue(t, "m4-b")
	leaseStore, outboxStore := setupDynamoStores(t)
	sessionID := mqttlocal.UniqueClientID("m4-sess")
	topic := "e2e/m4/roundtrip"

	// Bridge A: SQS-A -> outbox -> MQTT (long drain prevents drain before crash).
	sessA := setupMQTTSession(t, mqttlocal.UniqueClientID("m4-a"), connectivity.SessionEphemeral)
	mqttTxA := setupMQTTSender(t, sessA)
	sqsRxA := newSQSReceiver(t, queueA)

	sessCfgA := e2eFastSessionConfig(sessionID)
	sessCfgA.DrainStrategy = persistence.NewFixedPoll(30 * time.Second)

	rtA := goruntime.New(
		goruntime.WithInstanceID("m4-bridge-A"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)
	routeCfg := goruntime.RouteConfig{
		ID:       "m4-sqs-to-mqtt",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "mqtt-bind", Address: topic}),
		Bindings: []routing.DestinationBinding{{ID: "mqtt-bind", SessionID: sessionID}},
	}
	if err := rtA.AddRoute(routeCfg, sqsRxA, mqttTxA, sessA, &sessCfgA); err != nil {
		t.Fatalf("AddRoute A: %v", err)
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	if err := rtA.Start(ctxA); err != nil {
		t.Fatalf("Start A: %v", err)
	}

	sendToSQS(t, sqsClientA, queueA, `{"failover":"test"}`, nil)
	if err := rtA.WaitQuiescent(ctxA, goruntime.QuiescenceOptions{
		MinQuiet: 500 * time.Millisecond, Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("WaitQuiescent A: %v", err)
	}

	cancelA()
	_ = rtA.Stop(context.Background())

	// Bridge B: drains orphaned outbox -> MQTT; MQTT receiver -> SQS-B.
	sessB := setupMQTTSession(t, mqttlocal.UniqueClientID("m4-b"), connectivity.SessionEphemeral)
	if err := sessB.Reconcile(context.Background(), connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile B: %v", err)
	}
	waitSubReady(t, sessB)

	mqttTxB := setupMQTTSender(t, sessB)
	sessCfgB := e2eFastSessionConfig(sessionID)
	sessCfgB.Plan = connectivity.SessionPlan{
		Subscriptions:       []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
		ExpectedReceiverIDs: []string{"m4-rx"},
	}

	rtB := goruntime.New(
		goruntime.WithInstanceID("m4-bridge-B"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)

	if err := rtB.AddRoute(routeCfg, newSQSReceiver(t, queueA), mqttTxB, sessB, &sessCfgB); err != nil {
		t.Fatalf("AddRoute B route1: %v", err)
	}

	mqttToSqs := goruntime.RouteConfig{
		ID:                 "m4-mqtt-to-sqs",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "sqs-bind", Address: queueB}),
		SourceCapabilities: directHoldCaps,
	}
	if err := rtB.AddRoute(mqttToSqs, paho.NewReceiver("m4-rx", sessB), newSQSSender(t, queueB), nil, nil); err != nil {
		t.Fatalf("AddRoute B route2: %v", err)
	}

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	if err := rtB.Start(ctxB); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	defer func() { _ = rtB.Stop(context.Background()) }()

	bodies := pollSQS(t, sqsClientB, queueB, 1, 20*time.Second)
	if len(bodies) < 1 {
		t.Fatal("expected message in SQS-B after failover")
	}
}

// verifies ten rapid MQTT publishes all arrive in SQS without drops.
func TestE2E_MQTTToSQS_BackpressureSQSSlow(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "m5")
	topic := "e2e/m5/pressure"

	bridgeSess := setupMQTTSession(t, mqttlocal.UniqueClientID("m5-bridge"), connectivity.SessionEphemeral)
	if err := bridgeSess.Reconcile(context.Background(), connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubReady(t, bridgeSess)

	mqttRx := paho.NewReceiver("m5-rx", bridgeSess)
	sqsTx := newSQSSender(t, queueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("m5-bridge"),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)
	cfg := goruntime.RouteConfig{
		ID:                 "m5-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "sqs-bind", Address: queueURL}),
		SourceCapabilities: directHoldCaps,
	}
	if err := rt.AddRoute(cfg, mqttRx, sqsTx, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("m5-pub"), connectivity.SessionEphemeral)
	pubTx := paho.NewSender(pubSess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	const msgCount = 10
	for i := 0; i < msgCount; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("m5-msg-%d", i),
			Subject: topic,
			Payload: []byte(fmt.Sprintf(`{"seq":%d}`, i)),
		})
		if err := pubTx.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: topic}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	bodies := pollSQS(t, sqsClient, queueURL, msgCount, 15*time.Second)
	if len(bodies) < msgCount {
		t.Fatalf("expected %d SQS messages, got %d", msgCount, len(bodies))
	}
}

// waitSubReady waits for the broker to ACK all pending SUBSCRIBE frames.
// It deliberately does NOT require ServiceLevelFull because tests
// typically construct the receiver (handler) AFTER calling Reconcile;
// Full would require HandlersRegistered > 0 which has not happened yet.
func waitSubReady(t *testing.T, sess *paho.Session) {
	t.Helper()
	wait.Until(t, 5*time.Second, "subscriptions active", func() bool {
		h := sess.Health(context.Background())
		return h.Connected && h.SubscriptionsActive == h.SubscriptionsWanted
	})
}

package amqp091

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// ═══════════════════════════════════════════════
// AMQP 0-9-1 Edge Integration Tests (Part 2)
//
// Validates health transitions, exchange routing, reconcile plan,
// batch completeness, header edge cases, and prefetch behavior.
// ═══════════════════════════════════════════════

// TestIntegration_Edge_SessionHealthTransitions validates the Health struct
// at each lifecycle stage: before start, after start, after close.
func TestIntegration_Edge_SessionHealthTransitions(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	ep := rabbitmqlocal.Endpoint(t)

	sess := NewSession(SessionOptions{BrokerURL: ep}, connectivity.SessionEphemeral, logger)
	ctx := context.Background()

	h := sess.Health(ctx)
	if h.Connected {
		t.Error("should not be connected before Start")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Errorf("ServiceLevel before start = %v, want None", h.ServiceLevel)
	}

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	h = sess.Health(ctx)
	if !h.Connected {
		t.Error("should be connected after Start")
	}
	if !h.Ready {
		t.Error("should be ready after Start")
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Errorf("ServiceLevel after start = %v, want Full", h.ServiceLevel)
	}

	_ = sess.Close(ctx)

	h = sess.Health(ctx)
	if h.Connected {
		t.Error("should not be connected after Close")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Errorf("ServiceLevel after close = %v, want None", h.ServiceLevel)
	}

	assertLog091Contains(t, &buf, "amqp091: session close initiated")
}

// TestIntegration_Edge_ExchangeRouting validates end-to-end message flow
// through an exchange, binding, and queue declared via Reconcile.
func TestIntegration_Edge_ExchangeRouting(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	ep := rabbitmqlocal.Endpoint(t)

	queue := rabbitmqlocal.UniqueQueue("edge-exch-rt")
	exchange := rabbitmqlocal.UniqueExchange("edge-exch-rt-ex")
	routingKey := "route.edge"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := NewSession(SessionOptions{BrokerURL: ep}, connectivity.SessionEphemeral, logger)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{
			Topic: queue,
			Config: &Config{Subscription: SubscriptionParams{
				Exchange:   exchange,
				RoutingKey: routingKey,
			}},
		}},
		Publishers: []connectivity.PublisherPlan{{Topic: exchange}},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange: exchange, RoutingKey: routingKey, Session: sess,
	})
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "exchange-routed", Subject: routingKey, Payload: []byte("routed"),
	})}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv := NewReceiver(ReceiverConfig{
		QueueName: queue, PrefetchCount: 1, Session: sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	var received *messaging.Envelope
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			received = del.Envelope()
			_ = del.Ack(recvCtx)
			recvCancel()
			return nil
		})
	}()
	<-done

	if received == nil {
		t.Fatal("no message received")
	}
	if received.ID != "exchange-routed" {
		t.Errorf("ID = %q, want %q", received.ID, "exchange-routed")
	}

	assertLog091Contains(t, &buf, "amqp091: publishing", "amqp091: message received")
}

// TestIntegration_Edge_ReconcilePlan validates that Reconcile stores
// subscriptions and the Health report reflects the wanted count.
func TestIntegration_Edge_ReconcilePlan(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	ep := rabbitmqlocal.Endpoint(t)

	q1 := rabbitmqlocal.UniqueQueue("edge-recon-1")
	q2 := rabbitmqlocal.UniqueQueue("edge-recon-2")
	ex := rabbitmqlocal.UniqueExchange("edge-recon-ex")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sess := NewSession(SessionOptions{BrokerURL: ep}, connectivity.SessionEphemeral, logger)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: q1, Config: &Config{Subscription: SubscriptionParams{Exchange: ex, RoutingKey: q1}}},
			{Topic: q2, Config: &Config{Subscription: SubscriptionParams{Exchange: ex, RoutingKey: q2}}},
		},
		Publishers: []connectivity.PublisherPlan{{Topic: ex}},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	h := sess.Health(ctx)
	if h.SubscriptionsWanted != 2 {
		t.Errorf("SubscriptionsWanted = %d, want 2", h.SubscriptionsWanted)
	}
	if h.SubscriptionsActive != 2 {
		t.Errorf("SubscriptionsActive = %d, want 2", h.SubscriptionsActive)
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Errorf("ServiceLevel = %v, want Full", h.ServiceLevel)
	}

	assertLog091Contains(t, &buf, "amqp091: reconcile", "amqp091: reconcile done")
}

// TestIntegration_Edge_SendBatchAllReceived validates that all messages from
// a SendBatch call are received with correct unique IDs.
func TestIntegration_Edge_SendBatchAllReceived(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-batch")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sender := NewSender(SenderConfig{
		Exchange: e.exchange, RoutingKey: e.queue, Session: e.sess,
	})
	defer func() { _ = sender.Close(context.Background()) }()

	const msgCount = 5
	envs := make([]*messaging.Envelope, msgCount)
	wantIDs := make(map[string]bool, msgCount)
	for i := range envs {
		id := "batch-" + string(rune('A'+i))
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: id, Subject: e.queue, Payload: []byte("payload"),
		})
		wantIDs[id] = true
	}

	sent, err := sender.SendBatch(ctx, func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent != msgCount {
		t.Fatalf("sent %d, want %d", sent, msgCount)
	}

	recv := NewReceiver(ReceiverConfig{
		QueueName: e.queue, PrefetchCount: 10, Session: e.sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	ids := make(map[string]bool)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			ids[del.Envelope().ID] = true
			_ = del.Ack(recvCtx)
			if len(ids) >= msgCount {
				recvCancel()
			}
			return nil
		})
	}()
	<-done

	if len(ids) != msgCount {
		t.Errorf("received %d unique messages, want %d", len(ids), msgCount)
	}
	for id := range wantIDs {
		if !ids[id] {
			t.Errorf("missing expected message ID %q", id)
		}
	}

	assertLog091Contains(t, &buf, "amqp091: published", "amqp091: publish confirmed")
}

// TestIntegration_Edge_HeaderRoundTrip validates that custom headers
// including unicode and long values survive the send/receive cycle.
func TestIntegration_Edge_HeaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-headers")

	longValue := strings.Repeat("y", 4096)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "unicode-headers", Subject: e.queue, Payload: []byte("body"),
		Headers: map[string]any{
			"emoji":      "hello 🌍🚀",
			"cjk":        "你好世界",
			"long-value": longValue,
			"diacritics": "résumé café naïve",
		},
	})

	got := edge091SendRecv(t, e, env, 20*time.Second)

	if got.Headers()["emoji"] != "hello 🌍🚀" {
		t.Errorf("emoji header = %v", got.Headers()["emoji"])
	}
	if got.Headers()["cjk"] != "你好世界" {
		t.Errorf("cjk header = %v", got.Headers()["cjk"])
	}
	if got.Headers()["long-value"] != longValue {
		t.Errorf("long-value length = %d, want %d",
			len(got.Headers()["long-value"].(string)), len(longValue))
	}
	if got.Headers()["diacritics"] != "résumé café naïve" {
		t.Errorf("diacritics header = %v", got.Headers()["diacritics"])
	}

	assertLog091Contains(t, &buf, "amqp091: publishing", "amqp091: message received")
}

// TestIntegration_Edge_PrefetchHonored validates that PrefetchCount=1
// limits in-flight unacked messages to 1 at a time.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Send 3 messages → Consumer with PrefetchCount=1
//	Consumer holds each delivery, verifies only 1 at a time
//	Then acks, receives next
//
// ───────────────────────────────────────────────
func TestIntegration_Edge_PrefetchHonored(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-prefetch")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sender := NewSender(SenderConfig{
		Exchange: e.exchange, RoutingKey: e.queue, Session: e.sess,
	})
	defer func() { _ = sender.Close(context.Background()) }()

	const total = 3
	for i := 0; i < total; i++ {
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: "pf-" + string(rune('A'+i)), Subject: e.queue, Payload: []byte("x"),
		})}); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
	}

	recv := NewReceiver(ReceiverConfig{
		QueueName: e.queue, PrefetchCount: 1, Session: e.sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 15*time.Second)
	defer recvCancel()

	var mu sync.Mutex
	inflight := 0
	maxInflight := 0
	received := 0

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			mu.Lock()
			inflight++
			if inflight > maxInflight {
				maxInflight = inflight
			}
			mu.Unlock()

			// OTHER: intentional delay — gives broker time to deliver additional
			// messages so we can detect prefetch violations (max inflight > 1).
			time.Sleep(100 * time.Millisecond)

			mu.Lock()
			inflight--
			received++
			count := received
			mu.Unlock()

			_ = del.Ack(recvCtx)

			if count >= total {
				recvCancel()
			}
			return nil
		})
	}()
	<-done

	mu.Lock()
	gotMax := maxInflight
	gotTotal := received
	mu.Unlock()

	if gotTotal != total {
		t.Errorf("received %d, want %d", gotTotal, total)
	}
	if gotMax > 1 {
		t.Errorf("max inflight = %d, want <= 1 (PrefetchCount=1)", gotMax)
	}

	assertLog091Contains(t, &buf, "amqp091: message received")
}

package amqp091

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// TestIntegration_SessionHealth validates health reporting before start,
// after start, after reconcile, and after close.
func TestIntegration_SessionHealth(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)

	h := sess.Health(ctx)
	if h.Connected {
		t.Fatal("expected disconnected before Start")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Fatalf("service level = %q before Start, want %q", h.ServiceLevel, ports.ServiceLevelNone)
	}

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	h = sess.Health(ctx)
	if !h.Connected {
		t.Fatal("expected connected after Start")
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("service level = %q after Start (no subs), want %q", h.ServiceLevel, ports.ServiceLevelFull)
	}

	queueName := rabbitmqlocal.UniqueQueue("sess-health")
	exchangeName := rabbitmqlocal.UniqueExchange("sess-health-ex")

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{
				Topic: queueName,
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:   exchangeName,
					RoutingKey: queueName,
					Durable:    false,
				}},
			},
		},
		Publishers: []connectivity.PublisherPlan{
			{Topic: exchangeName, Config: &Config{Publisher: PublisherParams{Durable: false}}},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	h = sess.Health(ctx)
	if h.SubscriptionsWanted != 1 {
		t.Fatalf("SubscriptionsWanted = %d, want 1", h.SubscriptionsWanted)
	}
	if h.SubscriptionsActive != 1 {
		t.Fatalf("SubscriptionsActive = %d, want 1", h.SubscriptionsActive)
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("service level = %q after Reconcile, want %q", h.ServiceLevel, ports.ServiceLevelFull)
	}

	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	h = sess.Health(ctx)
	if h.Connected {
		t.Fatal("expected disconnected after Close")
	}
}

// TestIntegration_SessionEvents validates that SessionConnected and
// SessionReconciled events are emitted during session lifecycle.
func TestIntegration_SessionEvents(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	events := sess.Events()

	drainCtx, drainCancel := context.WithTimeout(ctx, 5*time.Second)
	defer drainCancel()

	var collected []ports.SessionEventType
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				drainCancel()
				goto checkConnected
			}
			collected = append(collected, ev.Type)
			if ev.Type == ports.SessionConnected {
				drainCancel()
				goto checkConnected
			}
		case <-drainCtx.Done():
			goto checkConnected
		}
	}
checkConnected:
	hasConnected := false
	for _, et := range collected {
		if et == ports.SessionConnected {
			hasConnected = true
		}
	}
	if !hasConnected {
		t.Fatal("SessionConnected event not received after Start")
	}

	queueName := rabbitmqlocal.UniqueQueue("sess-events")
	exchangeName := rabbitmqlocal.UniqueExchange("sess-events-ex")
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{
				Topic: queueName,
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:   exchangeName,
					RoutingKey: queueName,
				}},
			},
		},
		Publishers: []connectivity.PublisherPlan{
			{Topic: exchangeName},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	reconCtx, reconCancel := context.WithTimeout(ctx, 5*time.Second)
	defer reconCancel()

	hasReconciled := false
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				reconCancel()
				goto checkReconciled
			}
			if ev.Type == ports.SessionReconciled {
				hasReconciled = true
				reconCancel()
				goto checkReconciled
			}
		case <-reconCtx.Done():
			goto checkReconciled
		}
	}
checkReconciled:
	if !hasReconciled {
		t.Fatal("SessionReconciled event not received after Reconcile")
	}
}

// TestIntegration_SessionReconcile_MultipleQueues validates reconcile
// with multiple queues and different exchange types.
func TestIntegration_SessionReconcile_MultipleQueues(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	directQueue := rabbitmqlocal.UniqueQueue("multi-direct")
	directExchange := rabbitmqlocal.UniqueExchange("multi-direct-ex")
	fanoutQueue := rabbitmqlocal.UniqueQueue("multi-fanout")
	fanoutExchange := rabbitmqlocal.UniqueExchange("multi-fanout-ex")

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{
				Topic: directQueue,
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:   directExchange,
					RoutingKey: directQueue,
				}},
			},
			{
				Topic: fanoutQueue,
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:     fanoutExchange,
					ExchangeType: "fanout",
					RoutingKey:   "",
				}},
			},
		},
		Publishers: []connectivity.PublisherPlan{
			{Topic: directExchange},
			{Topic: fanoutExchange, Config: &Config{Publisher: PublisherParams{ExchangeType: "fanout"}}},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	directSender := NewSender(SenderConfig{
		Exchange:   directExchange,
		RoutingKey: directQueue,
		Session:    sess,
		Timeout:    10 * time.Second,
	})
	fanoutSender := NewSender(SenderConfig{
		Exchange: fanoutExchange,
		Session:  sess,
		Timeout:  10 * time.Second,
	})

	if err := directSender.Send(ctx, ports.OutboundMessage{Envelope: &messaging.Envelope{
		ID: "direct-1", Subject: directQueue, Payload: []byte("direct-payload"),
	}}); err != nil {
		t.Fatalf("send direct: %v", err)
	}
	if err := fanoutSender.Send(ctx, ports.OutboundMessage{Envelope: &messaging.Envelope{
		ID: "fanout-1", Subject: fanoutQueue, Payload: []byte("fanout-payload"),
	}}); err != nil {
		t.Fatalf("send fanout: %v", err)
	}

	directReceiver := NewReceiver(ReceiverConfig{
		QueueName:     directQueue,
		PrefetchCount: 1,
		Session:       sess,
	})
	fanoutReceiver := NewReceiver(ReceiverConfig{
		QueueName:     fanoutQueue,
		PrefetchCount: 1,
		Session:       sess,
	})

	directCh := make(chan ports.Delivery, 1)
	fanoutCh := make(chan ports.Delivery, 1)

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	go func() {
		_ = directReceiver.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			directCh <- d
			return nil
		})
	}()
	go func() {
		_ = fanoutReceiver.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			fanoutCh <- d
			return nil
		})
	}()

	for i := 0; i < 2; i++ {
		select {
		case d := <-directCh:
			if d.Envelope().ID != "direct-1" {
				t.Errorf("direct message ID = %q, want %q", d.Envelope().ID, "direct-1")
			}
			if err := d.Ack(ctx); err != nil {
				t.Errorf("ack direct: %v", err)
			}
		case d := <-fanoutCh:
			if d.Envelope().ID != "fanout-1" {
				t.Errorf("fanout message ID = %q, want %q", d.Envelope().ID, "fanout-1")
			}
			if err := d.Ack(ctx); err != nil {
				t.Errorf("ack fanout: %v", err)
			}
		case <-recvCtx.Done():
			t.Fatal("timed out waiting for messages from both queues")
		}
	}
}

// TestIntegration_SessionCloseAndRestart validates that a session can
// be closed and a new session started on the same broker.
func TestIntegration_SessionCloseAndRestart(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueName := rabbitmqlocal.UniqueQueue("sess-restart")
	exchangeName := rabbitmqlocal.UniqueExchange("sess-restart-ex")

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{
				Topic: queueName,
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:   exchangeName,
					RoutingKey: queueName,
				}},
			},
		},
		Publishers: []connectivity.PublisherPlan{
			{Topic: exchangeName},
		},
	}

	sess1 := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess1.Start(ctx); err != nil {
		t.Fatalf("session1 Start: %v", err)
	}
	if err := sess1.Reconcile(ctx, plan); err != nil {
		t.Fatalf("session1 Reconcile: %v", err)
	}

	sender1 := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess1,
		Timeout:    10 * time.Second,
	})
	if err := sender1.Send(ctx, ports.OutboundMessage{Envelope: &messaging.Envelope{
		ID: "restart-msg", Subject: queueName, Payload: []byte("first-session"),
	}}); err != nil {
		t.Fatalf("send on session1: %v", err)
	}

	if err := sess1.Close(ctx); err != nil {
		t.Fatalf("session1 Close: %v", err)
	}

	sess2 := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess2.Start(ctx); err != nil {
		t.Fatalf("session2 Start: %v", err)
	}
	defer func() { _ = sess2.Close(ctx) }()
	if err := sess2.Reconcile(ctx, plan); err != nil {
		t.Fatalf("session2 Reconcile: %v", err)
	}

	sender2 := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess2,
		Timeout:    10 * time.Second,
	})
	if err := sender2.Send(ctx, ports.OutboundMessage{Envelope: &messaging.Envelope{
		ID: "restart-msg-2", Subject: queueName, Payload: []byte("second-session"),
	}}); err != nil {
		t.Fatalf("send on session2: %v", err)
	}

	receiver := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 10,
		Session:       sess2,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	received := make(chan *messaging.Envelope, 2)
	go func() {
		_ = receiver.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			received <- d.Envelope()
			_ = d.Ack(recvCtx)
			return nil
		})
	}()

	got := 0
	ids := map[string]bool{}
	for got < 2 {
		select {
		case env := <-received:
			ids[env.ID] = true
			got++
		case <-recvCtx.Done():
			t.Fatalf("timed out: received %d/2 messages", got)
		}
	}

	if !ids["restart-msg"] {
		t.Error("missing message from first session")
	}
	if !ids["restart-msg-2"] {
		t.Error("missing message from second session")
	}
}

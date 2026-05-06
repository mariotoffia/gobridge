package amqp091

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

func TestMain(m *testing.M) {
	rabbitmqlocal.Configure(rabbitmqlocal.WithCleanOrphans(true))
	code := m.Run()
	rabbitmqlocal.Shutdown()
	os.Exit(code)
}

// verifies full send-receive-ack cycle through a real RabbitMQ broker.
func TestIntegration_SendReceive(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	queueName := rabbitmqlocal.UniqueQueue("integ-sr")
	exchangeName := rabbitmqlocal.UniqueExchange("integ-ex")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
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
			{
				Topic: exchangeName,
				Config: &Config{Publisher: PublisherParams{
					Durable: false,
				}},
			},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
		Timeout:    10 * time.Second,
	})

	payload := []byte(`{"integration":"test"}`)
	env := &messaging.Envelope{
		ID:      "integ-msg-001",
		Subject: queueName,
		Payload: payload,
		Headers: map[string]any{
			"tenant": "test-tenant",
		},
	}

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("send: %v", err)
	}

	receiver := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 1,
		Session:       sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	received := make(chan ports.Delivery, 1)
	go func() {
		_ = receiver.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			received <- d
			recvCancel()
			return nil
		})
	}()

	select {
	case del := <-received:
		gotEnv := del.Envelope()
		if gotEnv.ID != "integ-msg-001" {
			t.Errorf("received ID = %q, want %q", gotEnv.ID, "integ-msg-001")
		}
		if string(gotEnv.Payload) != string(payload) {
			t.Errorf("received Payload = %q", gotEnv.Payload)
		}

		if err := del.Ack(ctx); err != nil {
			t.Fatalf("ack: %v", err)
		}
	case <-recvCtx.Done():
		t.Fatal("timed out waiting for message")
	}
}

// verifies send-receive with batch sender through a real RabbitMQ broker.
func TestIntegration_SendBatch(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	queueName := rabbitmqlocal.UniqueQueue("integ-batch")
	exchangeName := rabbitmqlocal.UniqueExchange("integ-batch-ex")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
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
		t.Fatalf("reconcile: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
		Timeout:    10 * time.Second,
	})

	envs := []*messaging.Envelope{
		{ID: "batch-1", Subject: queueName, Payload: []byte("one")},
		{ID: "batch-2", Subject: queueName, Payload: []byte("two")},
		{ID: "batch-3", Subject: queueName, Payload: []byte("three")},
	}

	sent, err := sender.SendBatch(ctx, func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("send batch: %v", err)
	}
	if sent != 3 {
		t.Errorf("sent = %d, want 3", sent)
	}

	receiver := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 10,
		Session:       sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	var deliveries []ports.Delivery
	go func() {
		_ = receiver.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			deliveries = append(deliveries, d)
			if len(deliveries) >= 3 {
				recvCancel()
			}
			return nil
		})
	}()

	<-recvCtx.Done()

	if len(deliveries) != 3 {
		t.Fatalf("received %d messages, want 3", len(deliveries))
	}

	// Verify each sent payload was received.
	rxBodies := make(map[string]bool, 3)
	for _, d := range deliveries {
		rxBodies[string(d.Envelope().Payload)] = true
		if err := d.Ack(ctx); err != nil {
			t.Errorf("ack: %v", err)
		}
	}
	for _, want := range []string{"one", "two", "three"} {
		if !rxBodies[want] {
			t.Errorf("missing payload %q in received messages", want)
		}
	}
}

// verifies retry (nack+requeue) results in redelivery.
func TestIntegration_RetryRedelivers(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	queueName := rabbitmqlocal.UniqueQueue("integ-retry")
	exchangeName := rabbitmqlocal.UniqueExchange("integ-retry-ex")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
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
		t.Fatalf("reconcile: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
	})

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: &messaging.Envelope{
		ID: "retry-msg", Subject: queueName, Payload: []byte("retry-me"),
	}}); err != nil {
		t.Fatalf("send: %v", err)
	}

	receiver := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 1,
		Session:       sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 15*time.Second)
	defer recvCancel()

	attempt := 0
	go func() {
		_ = receiver.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			attempt++
			if attempt == 1 {
				return d.Retry(recvCtx, 0, errors.New("transient"))
			}
			env := d.Envelope()
			if !env.Headers[HeaderRedelivered].(bool) {
				t.Errorf("expected redelivered=true on second delivery")
			}
			_ = d.Ack(recvCtx)
			recvCancel()
			return nil
		})
	}()

	<-recvCtx.Done()

	if attempt < 2 {
		t.Fatalf("expected at least 2 delivery attempts, got %d", attempt)
	}
}

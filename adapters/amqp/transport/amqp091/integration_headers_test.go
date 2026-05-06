package amqp091

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// TestIntegration_HeaderRoundTrip validates all AMQP 0-9-1 properties
// are preserved through a send/receive cycle.
func TestIntegration_HeaderRoundTrip(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueName := rabbitmqlocal.UniqueQueue("hdr-round")
	exchangeName := rabbitmqlocal.UniqueExchange("hdr-round-ex")

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
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
		t.Fatalf("Reconcile: %v", err)
	}

	env := &messaging.Envelope{
		ID:      "hdr-msg-001",
		Subject: queueName,
		Payload: []byte(`{"round":"trip"}`),
		Headers: map[string]any{
			HeaderCorrelationID: "corr-123",
			HeaderContentType:   "application/json",
			HeaderReplyTo:       "reply-queue",
			HeaderType:          "test.event",
			HeaderAppID:         "gobridge-test",
			HeaderPriority:      uint8(5),
			"x-custom-key":      "custom-value",
		},
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
		Timeout:    10 * time.Second,
	})
	if err := sender.Send(ctx, env); err != nil {
		t.Fatalf("Send: %v", err)
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
		got := del.Envelope()
		h := got.Headers

		assertHeader(t, h, HeaderCorrelationID, "corr-123")
		assertHeader(t, h, HeaderContentType, "application/json")
		assertHeader(t, h, HeaderReplyTo, "reply-queue")
		assertHeader(t, h, HeaderType, "test.event")
		assertHeader(t, h, HeaderAppID, "gobridge-test")

		if pri, ok := h[HeaderPriority].(uint8); !ok || pri != 5 {
			t.Errorf("%s = %v (%T), want uint8(5)", HeaderPriority, h[HeaderPriority], h[HeaderPriority])
		}

		if v, ok := h["x-custom-key"].(string); !ok || v != "custom-value" {
			t.Errorf("x-custom-key = %v (%T), want %q", h["x-custom-key"], h["x-custom-key"], "custom-value")
		}

		if err := del.Ack(ctx); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	case <-recvCtx.Done():
		t.Fatal("timed out waiting for message")
	}
}

// TestIntegration_EnvelopeTTL validates that envelope TTL/expiration
// is preserved through send/receive.
func TestIntegration_EnvelopeTTL(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueName := rabbitmqlocal.UniqueQueue("hdr-ttl")
	exchangeName := rabbitmqlocal.UniqueExchange("hdr-ttl-ex")

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
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
		t.Fatalf("Reconcile: %v", err)
	}

	env := &messaging.Envelope{
		ID:        "ttl-msg-001",
		Subject:   queueName,
		Payload:   []byte("ttl-test"),
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
		Timeout:    10 * time.Second,
	})
	if err := sender.Send(ctx, env); err != nil {
		t.Fatalf("Send: %v", err)
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
		h := del.Envelope().Headers
		exp, ok := h[HeaderExpiration].(string)
		if !ok || exp == "" {
			t.Fatalf("%s not set or empty; headers: %v", HeaderExpiration, h)
		}
		t.Logf("expiration header value: %s", exp)

		if err := del.Ack(ctx); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	case <-recvCtx.Done():
		t.Fatal("timed out waiting for message")
	}
}

// TestIntegration_ExtendNotSupported validates that Extend returns
// shared.ErrNotSupported on a real delivery.
func TestIntegration_ExtendNotSupported(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueName := rabbitmqlocal.UniqueQueue("hdr-extend")
	exchangeName := rabbitmqlocal.UniqueExchange("hdr-extend-ex")

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		connectivity.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
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
		t.Fatalf("Reconcile: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
		Timeout:    10 * time.Second,
	})
	if err := sender.Send(ctx, &messaging.Envelope{
		ID: "extend-msg", Subject: queueName, Payload: []byte("extend-test"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
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
		err := del.Extend(ctx, time.Now().Add(5*time.Minute))
		if !errors.Is(err, shared.ErrNotSupported) {
			t.Fatalf("Extend returned %v, want %v", err, shared.ErrNotSupported)
		}
		if err := del.Ack(ctx); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	case <-recvCtx.Done():
		t.Fatal("timed out waiting for message")
	}
}

func assertHeader(t *testing.T, h map[string]any, key, want string) {
	t.Helper()
	got, ok := h[key].(string)
	if !ok || got != want {
		t.Errorf("%s = %v (%T), want %q", key, h[key], h[key], want)
	}
}

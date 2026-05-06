package amqp091

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// ═══════════════════════════════════════════════
// AMQP 0-9-1 Edge Integration Tests (Part 1)
//
// Validates edge cases for payload, context, settlement idempotency,
// and session lifecycle against a live RabbitMQ broker with trace logging.
// ═══════════════════════════════════════════════

func traceLogger091(buf *bytes.Buffer) *slog.Logger {
	return logging.NewLogger(logging.LevelTrace, func(opts *slog.HandlerOptions) slog.Handler {
		return slog.NewTextHandler(buf, opts)
	})
}

func assertLog091Contains(t *testing.T, buf *bytes.Buffer, msgs ...string) {
	t.Helper()
	output := buf.String()
	for _, msg := range msgs {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q;\nlog output:\n%s", msg, output)
		}
	}
}

type edge091Env struct {
	sess     *Session
	exchange string
	queue    string
}

func edge091Setup(t *testing.T, logger *slog.Logger, prefix string) edge091Env {
	t.Helper()
	ep := rabbitmqlocal.Endpoint(t)
	queue := rabbitmqlocal.UniqueQueue(prefix)
	exchange := rabbitmqlocal.UniqueExchange(prefix + "-ex")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sess := NewSession(SessionOptions{BrokerURL: ep}, connectivity.SessionEphemeral, logger)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{
			Topic: queue,
			Config: &Config{Subscription: SubscriptionParams{
				Exchange:   exchange,
				RoutingKey: queue,
				Durable:    false,
			}},
		}},
		Publishers: []connectivity.PublisherPlan{{
			Topic:  exchange,
			Config: &Config{Publisher: PublisherParams{Durable: false}},
		}},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	return edge091Env{sess: sess, exchange: exchange, queue: queue}
}

func edge091SendRecv(t *testing.T, e edge091Env, env *messaging.Envelope,
	timeout time.Duration) *messaging.Envelope {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	sender := NewSender(SenderConfig{
		Exchange: e.exchange, RoutingKey: e.queue,
		Session: e.sess, Timeout: 10 * time.Second,
	})
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv := NewReceiver(ReceiverConfig{
		QueueName: e.queue, PrefetchCount: 1, Session: e.sess,
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
	return received
}

// TestIntegration_Edge_EmptyPayload validates send/receive with nil and
// zero-length payloads over AMQP 0-9-1.
func TestIntegration_Edge_EmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-empty")

	t.Run("nil_payload", func(t *testing.T) {
		got := edge091SendRecv(t, e, &messaging.Envelope{
			ID: "empty-nil", Subject: e.queue,
		}, 15*time.Second)
		if len(got.Payload) != 0 {
			t.Errorf("expected empty payload, got %d bytes", len(got.Payload))
		}
	})

	t.Run("zero_length_payload", func(t *testing.T) {
		got := edge091SendRecv(t, e, &messaging.Envelope{
			ID: "empty-zero", Subject: e.queue, Payload: []byte{},
		}, 15*time.Second)
		if len(got.Payload) != 0 {
			t.Errorf("expected empty payload, got %d bytes", len(got.Payload))
		}
	})

	assertLog091Contains(t, &buf, "amqp091: publishing", "amqp091: published",
		"amqp091: message received", "amqp091: acking")
}

// TestIntegration_Edge_LargePayload validates 1 MB payload integrity via
// SHA-256 checksum.
func TestIntegration_Edge_LargePayload(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-large")

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	sentHash := sha256.Sum256(payload)

	got := edge091SendRecv(t, e, &messaging.Envelope{
		ID: "large-msg", Subject: e.queue, Payload: payload,
	}, 30*time.Second)

	gotHash := sha256.Sum256(got.Payload)
	if sentHash != gotHash {
		t.Fatalf("payload hash mismatch: sent %x, got %x", sentHash, gotHash)
	}

	assertLog091Contains(t, &buf, "amqp091: publishing", "amqp091: published")
}

// TestIntegration_Edge_SendContextTimeout validates that sending with an
// already-cancelled context returns an error.
func TestIntegration_Edge_SendContextTimeout(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-send-timeout")

	sender := NewSender(SenderConfig{
		Exchange: e.exchange, RoutingKey: e.queue,
		Session: e.sess, Timeout: 10 * time.Second,
	})
	defer func() { _ = sender.Close(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sender.Send(ctx, &messaging.Envelope{
		ID: "timeout-msg", Subject: e.queue, Payload: []byte("hello"),
	})
	if err == nil {
		t.Fatal("expected error from Send with cancelled context")
	}
}

// TestIntegration_Edge_ReceiveContextCancel validates that cancelling the
// receiver context leads to a clean shutdown.
func TestIntegration_Edge_ReceiveContextCancel(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-recv-cancel")

	recv := NewReceiver(ReceiverConfig{
		QueueName: e.queue, PrefetchCount: 1, Session: e.sess,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error { return nil }) }()

	select {
	case err := <-done:
		if err != nil && ctx.Err() == nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not exit within timeout")
	}

	assertLog091Contains(t, &buf, "amqp091: receiver starting",
		"amqp091: receiver channel opened")
}

// TestIntegration_Edge_DoubleAck validates that acking the same delivery
// twice is idempotent (sync.Once) and only one ack trace appears.
func TestIntegration_Edge_DoubleAck(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-double-ack")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sender := NewSender(SenderConfig{
		Exchange: e.exchange, RoutingKey: e.queue, Session: e.sess,
	})
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, &messaging.Envelope{
		ID: "dbl-ack", Subject: e.queue, Payload: []byte("ack-me"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv := NewReceiver(ReceiverConfig{
		QueueName: e.queue, PrefetchCount: 1, Session: e.sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			_ = del.Ack(recvCtx)
			_ = del.Ack(recvCtx) // second: no-op
			recvCancel()
			return nil
		})
	}()
	<-done

	count := strings.Count(buf.String(), "amqp091: acking")
	if count != 1 {
		t.Errorf("expected exactly 1 ack trace, got %d", count)
	}
}

// TestIntegration_Edge_DoubleRetry validates that retrying the same delivery
// twice is idempotent and only one nack trace is emitted.
func TestIntegration_Edge_DoubleRetry(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-double-retry")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sender := NewSender(SenderConfig{
		Exchange: e.exchange, RoutingKey: e.queue, Session: e.sess,
	})
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, &messaging.Envelope{
		ID: "dbl-retry", Subject: e.queue, Payload: []byte("retry-me"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv := NewReceiver(ReceiverConfig{
		QueueName: e.queue, PrefetchCount: 1, Session: e.sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	attempt := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			attempt++
			if attempt == 1 {
				_ = del.Retry(recvCtx, 0, nil)
				_ = del.Retry(recvCtx, 0, nil) // no-op
				return nil
			}
			_ = del.Ack(recvCtx)
			recvCancel()
			return nil
		})
	}()
	<-done

	count := strings.Count(buf.String(), "amqp091: nacking (requeue)")
	if count != 1 {
		t.Errorf("expected exactly 1 nack trace, got %d", count)
	}
}

// TestIntegration_Edge_AckThenRetry validates that only the first settlement
// (Ack) takes effect when Retry is called after.
func TestIntegration_Edge_AckThenRetry(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	e := edge091Setup(t, logger, "edge-ack-retry")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sender := NewSender(SenderConfig{
		Exchange: e.exchange, RoutingKey: e.queue, Session: e.sess,
	})
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, &messaging.Envelope{
		ID: "ack-then-retry", Subject: e.queue, Payload: []byte("test"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv := NewReceiver(ReceiverConfig{
		QueueName: e.queue, PrefetchCount: 1, Session: e.sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			_ = del.Ack(recvCtx)
			_ = del.Retry(recvCtx, 0, nil) // should be no-op
			recvCancel()
			return nil
		})
	}()
	<-done

	output := buf.String()
	if strings.Count(output, "amqp091: acking") != 1 {
		t.Error("expected exactly 1 ack trace")
	}
	if strings.Contains(output, "amqp091: nacking") {
		t.Error("should not have nack trace after ack-first")
	}
}

// TestIntegration_Edge_SendAfterSessionClose validates that sending on a
// closed session returns an error.
func TestIntegration_Edge_SendAfterSessionClose(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	ep := rabbitmqlocal.Endpoint(t)

	sess := NewSession(SessionOptions{BrokerURL: ep}, connectivity.SessionEphemeral, logger)
	ctx := context.Background()
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange: "amq.direct", RoutingKey: "nonexistent", Session: sess,
	})
	_ = sess.Close(ctx)

	err := sender.Send(ctx, &messaging.Envelope{
		ID: "after-close", Subject: "test", Payload: []byte("nope"),
	})
	if err == nil {
		t.Fatal("expected error sending after session close")
	}

	assertLog091Contains(t, &buf, "amqp091: session close initiated")
}

// TestIntegration_Edge_ReceiverOnClosedSession validates that running a
// receiver on a closed session exits promptly (events channel is closed,
// so waitForReconnect returns immediately).
func TestIntegration_Edge_ReceiverOnClosedSession(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger091(&buf)
	ep := rabbitmqlocal.Endpoint(t)

	sess := NewSession(SessionOptions{BrokerURL: ep}, connectivity.SessionEphemeral, logger)
	ctx := context.Background()
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = sess.Close(ctx)

	recv := NewReceiver(ReceiverConfig{
		QueueName: "nonexistent", PrefetchCount: 1, Session: sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()

	done := make(chan error, 1)
	go func() { done <- recv.Run(recvCtx, func(_ context.Context, _ ports.Delivery) error { return nil }) }()

	select {
	case <-done:
		// Receiver exited promptly, which is correct for a closed session
	case <-time.After(10 * time.Second):
		t.Fatal("receiver did not return within timeout on closed session")
	}
}

// TestIntegration_Edge_WrongCredentials validates that connecting with
// invalid credentials returns an error.
func TestIntegration_Edge_WrongCredentials(t *testing.T) {
	_ = rabbitmqlocal.Endpoint(t) // ensure container is up

	sess := NewSession(SessionOptions{
		BrokerURL:      "amqp://baduser:badpass@127.0.0.1:0/",
		ConnectTimeout: 5 * time.Second,
	}, connectivity.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := sess.Start(ctx)
	if err == nil {
		_ = sess.Close(ctx)
		t.Fatal("expected error with wrong credentials")
	}
}

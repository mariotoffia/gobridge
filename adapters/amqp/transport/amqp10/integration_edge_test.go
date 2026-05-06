package amqp10

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// ═══════════════════════════════════════════════
// AMQP 1.0 Edge Integration Tests (Part 1)
//
// Validates edge cases for payload, context, settlement idempotency,
// and session lifecycle against a live Artemis broker with trace logging.
// ═══════════════════════════════════════════════

func traceLogger(buf *bytes.Buffer) *slog.Logger {
	return logging.NewLogger(logging.LevelTrace, func(opts *slog.HandlerOptions) slog.Handler {
		return slog.NewTextHandler(buf, opts)
	})
}

func assertLogContains(t *testing.T, buf *bytes.Buffer, msgs ...string) {
	t.Helper()
	output := buf.String()
	for _, msg := range msgs {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q;\nlog output:\n%s", msg, output)
		}
	}
}

func edgeSession(t *testing.T, logger *slog.Logger) *Session {
	t.Helper()
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       pass,
		ConnectTimeout: 15 * time.Second,
		IdleTimeout:    1 * time.Minute,
	}, domain.SessionEphemeral, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("session Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

func edgeSendRecv(t *testing.T, sess *Session, addr string, env *domain.Envelope,
	timeout time.Duration) *domain.Envelope {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	sender, err := NewSender(SenderConfig{Address: addr, Session: sess, Timeout: 10 * time.Second}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv, err := NewReceiver(ReceiverConfig{Address: addr, LinkCredit: 10, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	var received *domain.Envelope
	_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		received = del.Envelope()
		_ = del.Ack(recvCtx)
		recvCancel()
		return nil
	})

	if received == nil {
		t.Fatal("no message received")
	}
	return received
}

// TestIntegration_Edge_EmptyPayload validates send/receive with nil and
// zero-length payloads, ensuring no panic or corruption.
func TestIntegration_Edge_EmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-empty")

	t.Run("nil_payload", func(t *testing.T) {
		got := edgeSendRecv(t, sess, addr, &domain.Envelope{
			ID: "empty-nil", Subject: "test",
		}, 15*time.Second)
		if len(got.Payload) != 0 {
			t.Errorf("expected empty payload, got %d bytes", len(got.Payload))
		}
	})

	t.Run("zero_length_payload", func(t *testing.T) {
		got := edgeSendRecv(t, sess, addr, &domain.Envelope{
			ID: "empty-zero", Subject: "test", Payload: []byte{},
		}, 15*time.Second)
		if len(got.Payload) != 0 {
			t.Errorf("expected empty payload, got %d bytes", len(got.Payload))
		}
	})

	assertLogContains(t, &buf, "amqp10: sending", "amqp10: send complete",
		"amqp10: received message", "amqp10: accepting message")
}

// TestIntegration_Edge_LargePayload validates integrity of a 1 MB message
// through send/receive using SHA-256 checksum.
func TestIntegration_Edge_LargePayload(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-large")

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	sentHash := sha256.Sum256(payload)

	got := edgeSendRecv(t, sess, addr, &domain.Envelope{
		ID: "large-msg", Subject: "test", Payload: payload,
	}, 30*time.Second)

	gotHash := sha256.Sum256(got.Payload)
	if sentHash != gotHash {
		t.Fatalf("payload hash mismatch: sent %x, got %x", sentHash, gotHash)
	}

	assertLogContains(t, &buf, "amqp10: sending", "amqp10: send complete")
}

// TestIntegration_Edge_SendContextTimeout validates that sending with an
// already-cancelled context returns an error promptly.
func TestIntegration_Edge_SendContextTimeout(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-send-timeout")

	sender, err := NewSender(SenderConfig{
		Address: addr, Session: sess, Timeout: 10 * time.Second,
	}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = sender.Send(ctx, &domain.Envelope{
		ID: "timeout-msg", Subject: "test", Payload: []byte("hello"),
	})
	if err == nil {
		t.Fatal("expected error from Send with cancelled context")
	}
}

// TestIntegration_Edge_ReceiveContextCancel validates that cancelling the
// receiver context leads to a clean shutdown without errors.
func TestIntegration_Edge_ReceiveContextCancel(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-recv-cancel")

	recv, err := NewReceiver(ReceiverConfig{
		Address: addr, LinkCredit: 10, Session: sess,
	}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		t.Fatal("should not receive any message")
		return nil
	})

	if err != nil && ctx.Err() == nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	assertLogContains(t, &buf, "amqp10: receiver starting", "amqp10: closing receiver link")
}

// TestIntegration_Edge_DoubleAck validates that acking the same delivery
// twice is idempotent (sync.Once) and only one accept trace is emitted.
func TestIntegration_Edge_DoubleAck(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-double-ack")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sender, err := NewSender(SenderConfig{Address: addr, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, &domain.Envelope{
		ID: "dbl-ack", Subject: "test", Payload: []byte("ack-me"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv, err := NewReceiver(ReceiverConfig{Address: addr, LinkCredit: 10, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("first Ack: %v", err)
		}
		if err := del.Ack(recvCtx); err != nil {
			t.Errorf("second Ack should be no-op but got: %v", err)
		}
		recvCancel()
		return nil
	})

	count := strings.Count(buf.String(), "amqp10: accepting message")
	if count != 1 {
		t.Errorf("expected exactly 1 accept trace, got %d", count)
	}
}

// TestIntegration_Edge_DoubleRetry validates that retrying the same delivery
// twice is idempotent and only one release trace is emitted.
func TestIntegration_Edge_DoubleRetry(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-double-retry")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sender, err := NewSender(SenderConfig{Address: addr, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, &domain.Envelope{
		ID: "dbl-retry", Subject: "test", Payload: []byte("retry-me"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv, err := NewReceiver(ReceiverConfig{Address: addr, LinkCredit: 10, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	attempt := 0
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

	count := strings.Count(buf.String(), "amqp10: releasing message")
	if count != 1 {
		t.Errorf("expected exactly 1 release trace, got %d", count)
	}
}

// TestIntegration_Edge_AckThenRetry validates that only the first settlement
// (Ack) takes effect when Retry is called after Ack on the same delivery.
func TestIntegration_Edge_AckThenRetry(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-ack-retry")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sender, err := NewSender(SenderConfig{Address: addr, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	if err := sender.Send(ctx, &domain.Envelope{
		ID: "ack-then-retry", Subject: "test", Payload: []byte("test"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv, err := NewReceiver(ReceiverConfig{Address: addr, LinkCredit: 10, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		_ = del.Ack(recvCtx)
		_ = del.Retry(recvCtx, 0, nil) // should be no-op
		recvCancel()
		return nil
	})

	output := buf.String()
	if strings.Count(output, "amqp10: accepting message") != 1 {
		t.Error("expected exactly 1 accept trace")
	}
	if strings.Contains(output, "amqp10: releasing message") {
		t.Error("should not have release trace after ack-first")
	}
}

// TestIntegration_Edge_SendAfterSessionClose validates that sending on a
// closed session returns an error.
func TestIntegration_Edge_SendAfterSessionClose(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("edge-closed-send")

	sess := NewSession(SessionOptions{
		Address: ep, Username: user, Password: pass,
		ConnectTimeout: 15 * time.Second,
	}, domain.SessionEphemeral, logger)

	ctx := context.Background()
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sender, err := NewSender(SenderConfig{Address: addr, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	_ = sess.Close(ctx)

	err = sender.Send(ctx, &domain.Envelope{
		ID: "after-close", Subject: "test", Payload: []byte("nope"),
	})
	if err == nil {
		t.Fatal("expected error sending after session close")
	}
	assertLogContains(t, &buf, "amqp10: session close initiated")
}

// TestIntegration_Edge_ReceiverOnClosedSession validates that running a
// receiver on a closed session returns an error promptly.
func TestIntegration_Edge_ReceiverOnClosedSession(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	addr := artemislocal.UniqueAddress("edge-closed-recv")

	sess := NewSession(SessionOptions{
		Address: ep, Username: user, Password: pass,
		ConnectTimeout: 15 * time.Second,
	}, domain.SessionEphemeral, logger)

	ctx := context.Background()
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = sess.Close(ctx)

	recv, err := NewReceiver(ReceiverConfig{Address: addr, LinkCredit: 10, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()

	err = recv.Run(recvCtx, func(_ context.Context, _ ports.Delivery) error { return nil })
	if err == nil {
		t.Fatal("expected error running receiver on closed session")
	}
}

// TestIntegration_Edge_WrongCredentials validates that connecting with
// invalid credentials returns a BridgeError.
func TestIntegration_Edge_WrongCredentials(t *testing.T) {
	ep := artemislocal.Endpoint(t)

	sess := NewSession(SessionOptions{
		Address:        ep,
		Username:       "wrong-user",
		Password:       "wrong-pass",
		ConnectTimeout: 10 * time.Second,
	}, domain.SessionEphemeral, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := sess.Start(ctx)
	if err == nil {
		_ = sess.Close(ctx)
		t.Fatal("expected auth error with wrong credentials")
	}

	if _, ok := shared.AsBridgeError(err); !ok {
		t.Fatalf("expected BridgeError, got %T: %v", err, err)
	}
}

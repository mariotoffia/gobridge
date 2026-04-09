// Validates receiver construction, message conversion, and context cancellation.
package amqp10

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func TestNewReceiver_Validates(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())

	_, err := NewReceiver(ReceiverConfig{}, sess)
	if err == nil {
		t.Fatal("NewReceiver() should fail with empty address")
	}
}

func TestNewReceiver_AppliesDefaults(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())

	r, err := NewReceiver(ReceiverConfig{Address: "queue/in"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}
	if r.cfg.LinkCredit != 10 {
		t.Fatalf("LinkCredit = %d, want 10 default", r.cfg.LinkCredit)
	}
	if r.metrics == nil {
		t.Fatal("metrics should be non-nil")
	}
}

func TestReceiver_ConvertMessage(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/input"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	subj := "order.placed"
	expiry := time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC)

	msg := &amqp.Message{
		Properties: &amqp.MessageProperties{
			MessageID:          "msg-convert-1",
			Subject:            &subj,
			AbsoluteExpiryTime: &expiry,
		},
		Data: [][]byte{[]byte("order-payload")},
		ApplicationProperties: map[string]any{
			"tenant": "acme",
		},
	}

	settler := newMockSettler()
	del := r.convertMessage(context.Background(), msg, nil)
	_ = settler

	env := del.Envelope()
	if env.ID != "msg-convert-1" {
		t.Fatalf("ID = %q, want %q", env.ID, "msg-convert-1")
	}
	if env.Subject != "order.placed" {
		t.Fatalf("Subject = %q, want %q", env.Subject, "order.placed")
	}
	if string(env.Payload) != "order-payload" {
		t.Fatalf("Payload = %q, want %q", env.Payload, "order-payload")
	}
	if !env.ExpiresAt.Equal(expiry) {
		t.Fatalf("ExpiresAt = %v, want %v", env.ExpiresAt, expiry)
	}
	if env.Headers["tenant"] != "acme" {
		t.Fatalf("Headers[tenant] = %v, want %q", env.Headers["tenant"], "acme")
	}
}

func TestReceiver_ConvertMessage_NoSubject(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/fallback"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	msg := &amqp.Message{
		Data: [][]byte{[]byte("data")},
	}

	del := r.convertMessage(context.Background(), msg, nil)
	if del.Envelope().Subject != "queue/fallback" {
		t.Fatalf("Subject = %q, want address fallback %q", del.Envelope().Subject, "queue/fallback")
	}
}

func TestReceiver_ConvertMessage_ValueBody(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/val"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	msg := &amqp.Message{
		Value: []byte("value-body"),
	}

	del := r.convertMessage(context.Background(), msg, nil)
	if string(del.Envelope().Payload) != "value-body" {
		t.Fatalf("Payload = %q, want %q", del.Envelope().Payload, "value-body")
	}
}

func TestReceiver_ConvertMessage_EmptyBody(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/empty"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	msg := &amqp.Message{}

	del := r.convertMessage(context.Background(), msg, nil)
	if len(del.Envelope().Payload) != 0 {
		t.Fatalf("Payload should be empty, got %d bytes", len(del.Envelope().Payload))
	}
}

func TestReceiver_ConvertMessage_NonStringMessageID(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/id"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	msg := &amqp.Message{
		Properties: &amqp.MessageProperties{
			MessageID: uint64(12345),
		},
		Data: [][]byte{[]byte("data")},
	}

	del := r.convertMessage(context.Background(), msg, nil)
	if del.Envelope().ID != "" {
		t.Fatalf("ID = %q, want empty for non-string MessageID", del.Envelope().ID)
	}
}

func TestReceiver_Run_ContextCancel(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/cancel"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runErr := r.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		t.Fatal("emit should not be called")
		return nil
	})

	if runErr == nil {
		t.Fatal("Run() should return error on cancelled context")
	}
	if !errors.Is(runErr, context.Canceled) {
		var be *domain.BridgeError
		if errors.As(runErr, &be) {
			if be.Code != domain.ErrCodeUnavailable {
				t.Fatalf("error code = %q, want %q", be.Code, domain.ErrCodeUnavailable)
			}
		}
	}
}

func TestReceiver_NilLogger(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, nil)

	r, err := NewReceiver(ReceiverConfig{Address: "queue/nil-log"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}
	if r == nil {
		t.Fatal("NewReceiver() returned nil")
	}
}

func TestReceiver_CustomMetrics(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, domain.SessionEphemeral, slog.Default())

	r, err := NewReceiver(ReceiverConfig{
		Address: "queue/metrics",
		Metrics: rec,
	}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}
	if r.metrics != rec {
		t.Fatal("metrics should use the provided RecordingExporter")
	}
}

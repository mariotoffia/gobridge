package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// verifies NewSender applies default timeout.
func TestNewSender_Defaults(t *testing.T) {
	s := NewSender(SenderConfig{})
	if s.cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", s.cfg.Timeout)
	}
}

// verifies Send returns ErrUnavailable when no session is set.
func TestSender_Send_NoSession(t *testing.T) {
	s := NewSender(SenderConfig{})

	env := &domain.Envelope{
		ID:      "e1",
		Payload: []byte("hello"),
	}
	err := s.Send(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for nil session")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if !errors.Is(be, domain.ErrUnavailable) {
		t.Errorf("expected ErrUnavailable, got code %s", be.Code)
	}
}

// verifies Send returns ErrUnavailable when session has no connection.
func TestSender_Send_NoConnection(t *testing.T) {
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		domain.SessionEphemeral,
		slog.Default(),
	)
	defer sess.Close(context.Background())

	s := NewSender(SenderConfig{Session: sess})

	env := &domain.Envelope{
		ID:      "e2",
		Payload: []byte("hello"),
	}
	err := s.Send(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for disconnected session")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// verifies SendBatch returns 0 sent when first message fails.
func TestSender_SendBatch_FirstFails(t *testing.T) {
	s := NewSender(SenderConfig{})

	envs := []*domain.Envelope{
		{ID: "e1", Payload: []byte("a")},
		{ID: "e2", Payload: []byte("b")},
	}
	sent, err := s.SendBatch(context.Background(), envs)
	if err == nil {
		t.Fatal("expected error")
	}
	if sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}
}

// verifies SendBatch returns empty batch with no error.
func TestSender_SendBatch_Empty(t *testing.T) {
	s := NewSender(SenderConfig{})

	sent, err := s.SendBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}
}

// verifies Sender satisfies ports.Sender and ports.BatchSender interfaces.
func TestSender_ImplementsInterfaces(t *testing.T) {
	var _ ports.Sender = (*Sender)(nil)
	var _ ports.BatchSender = (*Sender)(nil)
}

// verifies NewSender uses session logger when cfg.Logger is nil.
func TestNewSender_InheritsSessionLogger(t *testing.T) {
	logger := slog.Default()
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		domain.SessionEphemeral,
		logger,
	)
	defer sess.Close(context.Background())

	s := NewSender(SenderConfig{Session: sess})
	if s.logger != logger {
		t.Error("expected sender to inherit session logger")
	}
}

// verifies NewSender uses NoopExporter when nil metrics provided.
func TestNewSender_NilMetrics(t *testing.T) {
	s := NewSender(SenderConfig{})
	if s.metrics == nil {
		t.Fatal("metrics should be non-nil (NoopExporter)")
	}
}

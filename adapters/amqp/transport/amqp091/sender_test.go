package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
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

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e1",
		Payload: []byte("hello"),
	})
	err := s.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: "rk"})
	if err == nil {
		t.Fatal("expected error for nil session")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if !errors.Is(be, shared.ErrUnavailable) {
		t.Errorf("expected ErrUnavailable, got code %s", be.Code)
	}
}

// verifies Send returns ErrUnavailable when session has no connection.
func TestSender_Send_NoConnection(t *testing.T) {
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		connectivity.SessionEphemeral,
		slog.Default(),
	)
	defer func() { _ = sess.Close(context.Background()) }()
	s := NewSender(SenderConfig{Session: sess})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e2",
		Payload: []byte("hello"),
	})
	err := s.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: "rk"})
	if err == nil {
		t.Fatal("expected error for disconnected session")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// verifies SendBatch reports per-message send errors (and 0 successes) when sends fail, with no whole-batch error.
func TestSender_SendBatch_FirstFails(t *testing.T) {
	s := NewSender(SenderConfig{})

	envs := []*messaging.Envelope{
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("a")}),
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e2", Payload: []byte("b")}),
	}
	results, err := s.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e, Address: "rk"}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch must not return a whole-batch error, got %v", err)
	}
	if sent := batchSent(results); sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}
	if results[0].Err == nil {
		t.Fatal("expected the first message to carry its send error")
	}
}

// verifies SendBatch returns empty batch with no error.
func TestSender_SendBatch_Empty(t *testing.T) {
	s := NewSender(SenderConfig{})

	results, err := s.SendBatch(context.Background(), []ports.OutboundMessage(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sent := batchSent(results); sent != 0 {
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
		connectivity.SessionEphemeral,
		logger,
	)
	defer func() { _ = sess.Close(context.Background()) }()
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

// verifies Send returns shared.ErrInvalidTopic when neither cfg.RoutingKey
// nor msg.Address is set. Mirrors the MQTT sender's missing-topic guard.
func TestSender_Send_MissingRoutingTarget(t *testing.T) {
	s := NewSender(SenderConfig{})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x", Subject: "logical.subject", Payload: []byte("hi")})
	err := s.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	if err == nil {
		t.Fatal("expected error when neither cfg.RoutingKey nor msg.Address is set")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if !errors.Is(be, shared.ErrInvalidTopic) {
		t.Errorf("expected ErrInvalidTopic, got code %s", be.Code)
	}
}

// verifies Send returns shared.ErrInvalidPayload when the envelope is nil.
func TestSender_Send_NilEnvelope(t *testing.T) {
	s := NewSender(SenderConfig{RoutingKey: "rk"})

	err := s.Send(context.Background(), ports.OutboundMessage{Address: "rk"})
	if err == nil {
		t.Fatal("expected error for nil envelope")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if !errors.Is(be, shared.ErrInvalidPayload) {
		t.Errorf("expected ErrInvalidPayload, got code %s", be.Code)
	}
}

// batchSent counts the successful (nil-Err) entries in a SendBatch result.
func batchSent(results []ports.BatchResult) int {
	n := 0
	for _, r := range results {
		if r.Err == nil {
			n++
		}
	}
	return n
}

// Validates sender construction, message building, and error handling.
package amqp10

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

func TestNewSender_Validates(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	_, err := NewSender(SenderConfig{}, sess)
	if err == nil {
		t.Fatal("NewSender() should fail with empty address")
	}
}

func TestNewSender_AppliesDefaults(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/out"}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	if s.cfg.Timeout != 30*time.Second {
		t.Fatalf("Timeout = %v, want 30s default", s.cfg.Timeout)
	}
	if s.metrics == nil {
		t.Fatal("metrics should be non-nil")
	}
}

func TestSender_Send_NoSession(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/out"}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-1",
		Subject: "test",
		Payload: []byte("data"),
	})

	err = s.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	if err == nil {
		t.Fatal("Send() should fail when session is not connected")
	}

	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("error should be *shared.BridgeError, got %T", err)
	}
}

func TestSender_SendBatch_NoSession(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/out"}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}

	envs := []*messaging.Envelope{
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b-1", Payload: []byte("one")}),
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b-2", Payload: []byte("two")}),
	}

	results, err := s.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err == nil {
		t.Fatal("SendBatch() should fail when session is not connected")
	}
	if results != nil {
		t.Fatalf("results = %v, want nil (whole-batch failure)", results)
	}
}

func TestSender_BuildMessage(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	if _, err := NewSender(SenderConfig{Address: "queue/out"}, sess); err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}

	now := time.Now().UTC()
	expiry := now.Add(time.Hour)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        "build-1",
		Subject:   "evt.test",
		Payload:   []byte("payload-data"),
		CreatedAt: now,
		ExpiresAt: expiry,
		Headers: map[string]any{
			"custom-header": "custom-value",
		},
	})

	msg := envelopeToMessage(env)

	if len(msg.Data) != 1 || string(msg.Data[0]) != "payload-data" {
		t.Fatalf("Data = %v", msg.Data)
	}
	if msg.Properties == nil {
		t.Fatal("Properties should be non-nil")
	}
	if msg.Properties.MessageID != "build-1" {
		t.Fatalf("MessageID = %v, want %q", msg.Properties.MessageID, "build-1")
	}
	if msg.Properties.Subject == nil || *msg.Properties.Subject != "evt.test" {
		t.Fatalf("Subject = %v", msg.Properties.Subject)
	}
	if msg.Properties.CreationTime == nil || !msg.Properties.CreationTime.Equal(now) {
		t.Fatalf("CreationTime = %v", msg.Properties.CreationTime)
	}
	if msg.Properties.AbsoluteExpiryTime == nil || !msg.Properties.AbsoluteExpiryTime.Equal(expiry) {
		t.Fatalf("AbsoluteExpiryTime = %v", msg.Properties.AbsoluteExpiryTime)
	}
}

func TestSender_BuildMessage_NoExpiry(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	if _, err := NewSender(SenderConfig{Address: "queue/out"}, sess); err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "no-expiry",
		Payload: []byte("data"),
	})

	msg := envelopeToMessage(env)

	if msg.Properties.AbsoluteExpiryTime != nil {
		t.Fatal("AbsoluteExpiryTime should be nil when no expiry set")
	}
}

func TestSender_Close_NoLink(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/out"}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v, should be nil when no link exists", err)
	}
}

func TestSender_SendBatch_ContextCancel(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/out"}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	envs := []*messaging.Envelope{
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "c-1", Payload: []byte("one")}),
	}

	_, sendErr := s.SendBatch(ctx, func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if sendErr == nil {
		t.Fatal("SendBatch() should fail with cancelled context")
	}
}

func TestSender_NilLogger(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, nil)

	s, err := NewSender(SenderConfig{Address: "queue/out"}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	if s == nil {
		t.Fatal("NewSender() returned nil")
	}
}

func TestSender_CustomMetrics(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{
		Address: "queue/out",
		Metrics: rec,
	}, sess)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	if s.metrics != rec {
		t.Fatal("metrics should use the provided RecordingExporter")
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

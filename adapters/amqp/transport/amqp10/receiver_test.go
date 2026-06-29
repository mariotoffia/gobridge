// Validates receiver construction, message conversion, and context cancellation.
package amqp10

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestNewReceiver_Validates(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

	_, err := NewReceiver(ReceiverConfig{}, sess)
	if err == nil {
		t.Fatal("NewReceiver() should fail with empty address")
	}
}

func TestNewReceiver_AppliesDefaults(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

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
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/input"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	subj := "order.placed"
	// Clock-relative absolute expiry (not a fragile hard-coded date):
	// messageToEnvelope stamps the broker AbsoluteExpiryTime onto the envelope
	// at construction; assert it is carried through unchanged.
	expiry := r.clock().Now().Add(24 * time.Hour)

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
	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	_ = settler

	if env.ID() != "msg-convert-1" {
		t.Fatalf("ID = %q, want %q", env.ID(), "msg-convert-1")
	}
	if env.Subject() != "order.placed" {
		t.Fatalf("Subject = %q, want %q", env.Subject(), "order.placed")
	}
	if string(env.Payload()) != "order-payload" {
		t.Fatalf("Payload = %q, want %q", env.Payload(), "order-payload")
	}
	if !env.ExpiresAt().Equal(expiry) {
		t.Fatalf("ExpiresAt = %v, want %v", env.ExpiresAt(), expiry)
	}
	if env.Headers()["tenant"] != "acme" {
		t.Fatalf("Headers[tenant] = %v, want %q", env.Headers()["tenant"], "acme")
	}
}

func TestReceiver_ConvertMessage_NoSubject(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/fallback"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	msg := &amqp.Message{
		Data: [][]byte{[]byte("data")},
	}

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	if env.Subject() != "" {
		t.Fatalf("Subject = %q, want empty (no fallback to link address)", env.Subject())
	}
}

func TestReceiver_ConvertMessage_ValueBody(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/val"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	msg := &amqp.Message{
		Value: []byte("value-body"),
	}

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	if string(env.Payload()) != "value-body" {
		t.Fatalf("Payload = %q, want %q", env.Payload(), "value-body")
	}
}

func TestReceiver_ConvertMessage_EmptyBody(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/empty"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	msg := &amqp.Message{}

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	if len(env.Payload()) != 0 {
		t.Fatalf("Payload should be empty, got %d bytes", len(env.Payload()))
	}
}

func TestReceiver_ConvertMessage_NonStringMessageID(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
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

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	if env.ID() == "" {
		t.Fatal("ID should be auto-generated for non-string MessageID")
	}
}

func TestReceiver_Run_ContextCancel(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
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
		var be *shared.BridgeError
		if errors.As(runErr, &be) {
			if be.Code != shared.ErrCodeUnavailable {
				t.Fatalf("error code = %q, want %q", be.Code, shared.ErrCodeUnavailable)
			}
		}
	}
}

// Conformance (ports.Receiver emit-error contract): when emit returns an
// error the receive loop surfaces it from Run and does NOT settle the
// delivery — the AMQP 1.0 message is neither Accepted nor
// Released/Modified — so it is left to the broker's redelivery policy.
// Settlement remains the exclusive responsibility of the processing
// pipeline via the Delivery handle, which is still usable afterwards
// (proving ownership did not transfer to the receiver).
func TestReceiver_EmitError_DeliveryNotSettled(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
	r, err := NewReceiver(ReceiverConfig{Address: "queue/emit-err"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	settler := newMockSettler()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "emit-err-amqp10"})
	del := NewDelivery(env, &amqp.Message{}, settler, slog.Default(), &ports.NoopExporter{}, r.clock())

	// Seed the receive loop with a fake link that yields the delivery once.
	r.link = &fakeLink{deliveries: []*Delivery{del}}

	sentinel := errors.New("pipeline rejected delivery")
	runErr := r.receiveLoop(context.Background(), func(_ context.Context, _ ports.Delivery) error {
		return sentinel
	})

	if !errors.Is(runErr, sentinel) {
		t.Fatalf("emit error must surface from Run, got %v", runErr)
	}

	settler.mu.Lock()
	accept, release, modify := settler.acceptCalls, settler.releaseCalls, settler.modifyCalls
	settler.mu.Unlock()
	if accept != 0 || release != 0 || modify != 0 {
		t.Fatalf("delivery must not be settled on emit error (accept=%d release=%d modify=%d)",
			accept, release, modify)
	}

	// Ownership stays with the pipeline: the delivery is still settleable.
	if err := del.Ack(context.Background()); err != nil {
		t.Fatalf("delivery should remain settleable after emit error, Ack: %v", err)
	}
	settler.mu.Lock()
	defer settler.mu.Unlock()
	if settler.acceptCalls != 1 {
		t.Fatalf("expected pipeline-driven Ack to accept exactly once, got %d", settler.acceptCalls)
	}
}

func TestReceiver_NilLogger(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, nil)

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
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())

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

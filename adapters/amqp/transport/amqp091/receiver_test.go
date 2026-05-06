package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// verifies deliveryToEnvelope converts an amqp.Delivery to a messaging.Envelope
// with correct field mappings. The transport routing key MUST NOT be promoted
// to Envelope.Subject — it is recorded under HeaderRoutingKey.
func TestReceiver_ConvertMessage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)

	d := amqp.Delivery{
		MessageId:  "msg-500",
		RoutingKey: "order.placed",
		Body:       []byte(`{"item":"widget"}`),
		Timestamp:  now,
		Headers: amqp.Table{
			"tenant":               "acme",
			HeaderGobridgeSubject:  "order.created",
		},
	}

	env := deliveryToEnvelope(d, nil)

	if env.ID != "msg-500" {
		t.Errorf("ID = %q, want %q", env.ID, "msg-500")
	}
	if env.Subject != "order.created" {
		t.Errorf("Subject = %q, want %q (must come only from gobridge.subject)",
			env.Subject, "order.created")
	}
	if string(env.Payload) != `{"item":"widget"}` {
		t.Errorf("Payload = %q", env.Payload)
	}
	if env.CreatedAt != now {
		t.Errorf("CreatedAt = %v, want %v", env.CreatedAt, now)
	}
	if env.Headers == nil {
		t.Fatal("Headers is nil")
	}
	if env.Headers["tenant"] != "acme" {
		t.Errorf("Headers[tenant] = %v", env.Headers["tenant"])
	}
	if env.Headers[HeaderRoutingKey] != "order.placed" {
		t.Errorf("Headers[%s] = %v, want %q",
			HeaderRoutingKey, env.Headers[HeaderRoutingKey], "order.placed")
	}
	if _, ok := env.Headers[HeaderGobridgeSubject]; ok {
		t.Errorf("Headers[%s] should be absent — typed extraction wins over generic pass-through",
			HeaderGobridgeSubject)
	}
}

// verifies deliveryToEnvelope generates an ID when MessageId is empty.
func TestReceiver_ConvertMessage_GeneratesID(t *testing.T) {
	d := amqp.Delivery{
		RoutingKey: "evt.test",
		Body:       []byte("body"),
	}

	env := deliveryToEnvelope(d, nil)

	if env.ID == "" {
		t.Error("expected auto-generated ID")
	}
	if len(env.ID) != 32 {
		t.Errorf("generated ID length = %d, want 32 hex chars", len(env.ID))
	}
}

// verifies deliveryToEnvelope uses time.Now when Timestamp is zero.
func TestReceiver_ConvertMessage_ZeroTimestamp(t *testing.T) {
	before := time.Now()
	d := amqp.Delivery{
		MessageId:  "msg-ts",
		RoutingKey: "evt",
	}

	env := deliveryToEnvelope(d, nil)

	if env.CreatedAt.Before(before) {
		t.Error("CreatedAt should be >= time.Now() at call time")
	}
}

// verifies Receiver.Run exits when the context is cancelled.
func TestReceiver_Run_ContextCancel(t *testing.T) {
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		connectivity.SessionEphemeral,
		slog.Default(),
	)
	defer func() { _ = sess.Close(context.Background()) }()
	r := NewReceiver(ReceiverConfig{
		QueueName: "test-queue",
		Session:   sess,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := r.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		t.Fatal("emit should not be called")
		return nil
	})

	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// verifies NewReceiver uses NoopExporter when nil metrics provided.
func TestNewReceiver_NilMetrics(t *testing.T) {
	r := NewReceiver(ReceiverConfig{})
	if r.metrics == nil {
		t.Fatal("metrics should be non-nil (NoopExporter)")
	}
}

// verifies NewReceiver inherits session logger when cfg.Logger is nil.
func TestNewReceiver_InheritsSessionLogger(t *testing.T) {
	logger := slog.Default()
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		connectivity.SessionEphemeral,
		logger,
	)
	defer func() { _ = sess.Close(context.Background()) }()
	r := NewReceiver(ReceiverConfig{Session: sess})
	if r.logger != logger {
		t.Error("expected receiver to inherit session logger")
	}
}

// verifies Receiver satisfies the ports.Receiver interface.
func TestReceiver_ImplementsInterface(t *testing.T) {
	var _ ports.Receiver = (*Receiver)(nil)
}

// verifies generateConsumerTag returns a non-empty string with the expected prefix.
func TestGenerateConsumerTag(t *testing.T) {
	tag := generateConsumerTag()
	if tag == "" {
		t.Fatal("expected non-empty consumer tag")
	}
	if len(tag) < len("gobridge-") {
		t.Fatalf("tag %q too short", tag)
	}
	if tag[:9] != "gobridge-" {
		t.Errorf("tag prefix = %q, want %q", tag[:9], "gobridge-")
	}
}

// verifies generateEnvelopeID returns a 32-char hex string.
func TestGenerateEnvelopeID(t *testing.T) {
	id := generateEnvelopeID()
	if len(id) != 32 {
		t.Errorf("len(id) = %d, want 32", len(id))
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("non-hex character %c in ID %q", c, id)
		}
	}
}

// verifies two generated envelope IDs are distinct.
func TestGenerateEnvelopeID_Unique(t *testing.T) {
	a := generateEnvelopeID()
	b := generateEnvelopeID()
	if a == b {
		t.Fatalf("expected unique IDs, got %q twice", a)
	}
}

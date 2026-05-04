package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func makeTestDelivery(acker *mockAcknowledger, tag uint64) (*Delivery, *domain.Envelope) {
	env := &domain.Envelope{
		ID:      "test-env",
		Subject: "test.subject",
		Payload: []byte("hello"),
	}
	raw := amqp.Delivery{
		Acknowledger: acker,
		DeliveryTag:  tag,
		RoutingKey:   "test.subject",
	}
	return NewDelivery(env, raw, slog.Default(), &ports.NoopExporter{}, nil), env
}

// verifies Delivery.Envelope returns the stored envelope.
func TestDelivery_Envelope(t *testing.T) {
	del, env := makeTestDelivery(newMockAcknowledger(), 1)

	got := del.Envelope()
	if got != env {
		t.Fatal("Envelope() returned a different pointer")
	}
	if got.ID != "test-env" {
		t.Errorf("Envelope.ID = %q", got.ID)
	}
}

// verifies Delivery.Ack calls the underlying acknowledger once.
func TestDelivery_Ack(t *testing.T) {
	acker := newMockAcknowledger()
	del, _ := makeTestDelivery(acker, 7)

	if err := del.Ack(context.Background()); err != nil {
		t.Fatalf("Ack returned error: %v", err)
	}

	if acker.AckCalls != 1 {
		t.Errorf("AckCalls = %d, want 1", acker.AckCalls)
	}
}

// verifies Delivery.Ack propagates acknowledger errors as mapped BridgeErrors.
func TestDelivery_Ack_Error(t *testing.T) {
	acker := newMockAcknowledger()
	acker.AckFn = func(uint64, bool) error {
		return &amqp.Error{Code: 504, Reason: "channel error"}
	}
	del, _ := makeTestDelivery(acker, 1)

	err := del.Ack(context.Background())
	if err == nil {
		t.Fatal("expected error from Ack")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// verifies Delivery.Retry nacks with requeue=true for immediate redelivery.
func TestDelivery_Retry_Immediate(t *testing.T) {
	acker := newMockAcknowledger()
	var capturedRequeue bool
	acker.NackFn = func(_ uint64, _ bool, requeue bool) error {
		capturedRequeue = requeue
		return nil
	}
	del, _ := makeTestDelivery(acker, 3)

	err := del.Retry(context.Background(), 0, nil)
	if err != nil {
		t.Fatalf("Retry returned error: %v", err)
	}
	if acker.NackCalls != 1 {
		t.Errorf("NackCalls = %d, want 1", acker.NackCalls)
	}
	if !capturedRequeue {
		t.Error("expected requeue=true")
	}
}

// verifies Delivery.Retry with a delay still nacks immediately (logs a warning).
func TestDelivery_Retry_Delayed(t *testing.T) {
	acker := newMockAcknowledger()
	del, _ := makeTestDelivery(acker, 5)

	reason := errors.New("transient failure")
	err := del.Retry(context.Background(), 5*time.Second, reason)
	if err != nil {
		t.Fatalf("Retry returned error: %v", err)
	}
	if acker.NackCalls != 1 {
		t.Errorf("NackCalls = %d, want 1", acker.NackCalls)
	}
}

// verifies Delivery.Extend returns ErrNotSupported.
func TestDelivery_Extend(t *testing.T) {
	del, _ := makeTestDelivery(newMockAcknowledger(), 1)

	err := del.Extend(context.Background(), time.Now().Add(time.Minute))
	if !errors.Is(err, domain.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

// verifies second Ack is a no-op due to sync.Once.
func TestDelivery_DoubleAck(t *testing.T) {
	acker := newMockAcknowledger()
	del, _ := makeTestDelivery(acker, 10)

	if err := del.Ack(context.Background()); err != nil {
		t.Fatalf("first Ack error: %v", err)
	}
	if err := del.Ack(context.Background()); err != nil {
		t.Fatalf("second Ack error: %v", err)
	}

	if acker.AckCalls != 1 {
		t.Errorf("AckCalls = %d, want 1 (once guard)", acker.AckCalls)
	}
}

// verifies Ack then Retry is a no-op for the second call.
func TestDelivery_AckThenRetry(t *testing.T) {
	acker := newMockAcknowledger()
	del, _ := makeTestDelivery(acker, 11)

	_ = del.Ack(context.Background())
	_ = del.Retry(context.Background(), 0, nil)

	if acker.AckCalls != 1 {
		t.Errorf("AckCalls = %d, want 1", acker.AckCalls)
	}
	if acker.NackCalls != 0 {
		t.Errorf("NackCalls = %d, want 0", acker.NackCalls)
	}
}

// verifies NewDelivery uses a NoopExporter when nil metrics are provided.
func TestDelivery_NilMetrics(t *testing.T) {
	env := &domain.Envelope{ID: "x"}
	raw := amqp.Delivery{Acknowledger: newMockAcknowledger()}
	del := NewDelivery(env, raw, nil, nil, nil)

	if del.metrics == nil {
		t.Fatal("metrics should be non-nil (NoopExporter)")
	}
}

// verifies Delivery satisfies the ports.Delivery interface.
func TestDelivery_ImplementsInterface(t *testing.T) {
	var _ ports.Delivery = (*Delivery)(nil)
}

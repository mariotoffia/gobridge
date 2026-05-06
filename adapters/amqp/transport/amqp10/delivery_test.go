// Validates delivery settlement operations mapping to AMQP 1.0 dispositions.
package amqp10

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func newTestDelivery(settle settler) *Delivery {
	env := &domain.Envelope{
		ID:      "env-1",
		Subject: "test/topic",
		Payload: []byte("hello"),
	}
	msg := &amqp.Message{Data: [][]byte{[]byte("hello")}}
	return NewDelivery(env, msg, settle, slog.Default(), &ports.NoopExporter{}, nil)
}

func TestDelivery_Envelope(t *testing.T) {
	s := newMockSettler()
	d := newTestDelivery(s)

	env := d.Envelope()
	if env == nil {
		t.Fatal("Envelope() returned nil")
	}
	if env.ID != "env-1" {
		t.Fatalf("Envelope().ID = %q, want %q", env.ID, "env-1")
	}
	if env.Subject != "test/topic" {
		t.Fatalf("Envelope().Subject = %q, want %q", env.Subject, "test/topic")
	}
}

func TestDelivery_Ack(t *testing.T) {
	s := newMockSettler()
	d := newTestDelivery(s)

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acceptCalls != 1 {
		t.Fatalf("AcceptMessage called %d times, want 1", s.acceptCalls)
	}
}

func TestDelivery_Ack_Error(t *testing.T) {
	s := newMockSettler()
	s.acceptErr = errors.New("connection reset")
	d := newTestDelivery(s)

	err := d.Ack(context.Background())
	if err == nil {
		t.Fatal("Ack() should return error when AcceptMessage fails")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("error should be *shared.BridgeError, got %T", err)
	}
}

func TestDelivery_Retry_Immediate(t *testing.T) {
	s := newMockSettler()
	d := newTestDelivery(s)

	if err := d.Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry(0) error = %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releaseCalls != 1 {
		t.Fatalf("ReleaseMessage called %d times, want 1", s.releaseCalls)
	}
	if s.modifyCalls != 0 {
		t.Fatalf("ModifyMessage called %d times, want 0", s.modifyCalls)
	}
}

func TestDelivery_Retry_Delayed(t *testing.T) {
	s := newMockSettler()
	d := newTestDelivery(s)

	if err := d.Retry(context.Background(), 5*time.Second, nil); err != nil {
		t.Fatalf("Retry(5s) error = %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modifyCalls != 1 {
		t.Fatalf("ModifyMessage called %d times, want 1", s.modifyCalls)
	}
	if s.releaseCalls != 0 {
		t.Fatalf("ReleaseMessage called %d times, want 0", s.releaseCalls)
	}
	if s.lastModifyOpts == nil {
		t.Fatal("ModifyMessageOptions should be non-nil")
	}
	if !s.lastModifyOpts.DeliveryFailed {
		t.Fatal("DeliveryFailed should be true")
	}
	if s.lastModifyOpts.UndeliverableHere {
		t.Fatal("UndeliverableHere should be false")
	}
}

func TestDelivery_Extend(t *testing.T) {
	s := newMockSettler()
	d := newTestDelivery(s)

	err := d.Extend(context.Background(), time.Now().Add(time.Minute))
	if err == nil {
		t.Fatal("Extend() should return error")
	}
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("Extend() error = %v, want ErrNotSupported", err)
	}
}

func TestDelivery_DoubleAck(t *testing.T) {
	s := newMockSettler()
	d := newTestDelivery(s)

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("first Ack() error = %v", err)
	}
	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("second Ack() error = %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acceptCalls != 1 {
		t.Fatalf("AcceptMessage called %d times, want 1 (idempotent via sync.Once)", s.acceptCalls)
	}
}

func TestDelivery_AckThenRetry(t *testing.T) {
	s := newMockSettler()
	d := newTestDelivery(s)

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if err := d.Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acceptCalls != 1 {
		t.Fatalf("AcceptMessage called %d times, want 1", s.acceptCalls)
	}
	if s.releaseCalls != 0 {
		t.Fatalf("ReleaseMessage called %d times, want 0 (Once already fired)", s.releaseCalls)
	}
}

func TestDelivery_NilMetrics(t *testing.T) {
	env := &domain.Envelope{ID: "nil-metrics"}
	msg := &amqp.Message{}
	s := newMockSettler()

	d := NewDelivery(env, msg, s, slog.Default(), nil, nil)
	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack() with nil metrics error = %v", err)
	}
}

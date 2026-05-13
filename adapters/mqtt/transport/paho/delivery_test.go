package paho

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// verifies Delivery.Envelope returns the same envelope pointer passed to NewDelivery.
func TestDelivery_Envelope(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "t", Payload: []byte("p")})
	del := NewDelivery(env)

	got := del.Envelope()
	if got != env {
		t.Error("Envelope() should return the same pointer")
	}
}

// verifies Ack is a no-op and returns nil for MQTT at-most-once semantics.
func TestDelivery_AckIsNoop(t *testing.T) {
	del := NewDelivery(&messaging.Envelope{})
	if err := del.Ack(context.Background()); err != nil {
		t.Errorf("Ack() = %v, want nil", err)
	}
}

// verifies Retry returns ErrNotSupported.
func TestDelivery_RetryNotSupported(t *testing.T) {
	del := NewDelivery(&messaging.Envelope{})
	err := del.Retry(context.Background(), time.Second, errors.New("reason"))
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Errorf("Retry() = %v, want ErrNotSupported", err)
	}
}

// verifies Extend returns ErrNotSupported.
func TestDelivery_ExtendNotSupported(t *testing.T) {
	del := NewDelivery(&messaging.Envelope{})
	err := del.Extend(context.Background(), time.Now().Add(time.Minute))
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Errorf("Extend() = %v, want ErrNotSupported", err)
	}
}

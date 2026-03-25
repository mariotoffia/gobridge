package paho

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

func TestDelivery_Envelope(t *testing.T) {
	env := &domain.Envelope{ID: "e1", Subject: "t", Payload: []byte("p")}
	del := NewDelivery(env)

	got := del.Envelope()
	if got != env {
		t.Error("Envelope() should return the same pointer")
	}
}

func TestDelivery_AckIsNoop(t *testing.T) {
	del := NewDelivery(&domain.Envelope{})
	if err := del.Ack(context.Background()); err != nil {
		t.Errorf("Ack() = %v, want nil", err)
	}
}

func TestDelivery_RetryNotSupported(t *testing.T) {
	del := NewDelivery(&domain.Envelope{})
	err := del.Retry(context.Background(), time.Second, errors.New("reason"))
	if !errors.Is(err, domain.ErrNotSupported) {
		t.Errorf("Retry() = %v, want ErrNotSupported", err)
	}
}

func TestDelivery_ExtendNotSupported(t *testing.T) {
	del := NewDelivery(&domain.Envelope{})
	err := del.Extend(context.Background(), time.Now().Add(time.Minute))
	if !errors.Is(err, domain.ErrNotSupported) {
		t.Errorf("Extend() = %v, want ErrNotSupported", err)
	}
}

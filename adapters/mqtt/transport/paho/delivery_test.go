package paho

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

// verifies Ack without an ack callback (QoS 0 / legacy path) is a no-op
// returning nil.
func TestDelivery_AckWithoutCallbackIsNoop(t *testing.T) {
	del := NewDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{}))
	if err := del.Ack(context.Background()); err != nil {
		t.Errorf("Ack() = %v, want nil", err)
	}
}

// verifies Ack invokes the protocol-ack callback exactly once — even
// under concurrent Ack calls — and that subsequent calls return the
// first result (idempotent settlement).
func TestDelivery_AckInvokesCallbackExactlyOnce(t *testing.T) {
	var calls atomic.Int32
	wantErr := errors.New("broker gone")
	del := NewDelivery(
		messaging.MustEnvelope(messaging.EnvelopeInput{}),
		WithAckFunc(func() error {
			calls.Add(1)
			return wantErr
		}),
	)

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-barrier
			errs[idx] = del.Ack(context.Background())
		}(i)
	}
	close(barrier)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("ack callback invoked %d times, want exactly 1", got)
	}
	for i, err := range errs {
		if !errors.Is(err, wantErr) {
			t.Errorf("Ack call %d = %v, want the first result %v", i, err, wantErr)
		}
	}
}

// verifies a successful ack result is sticky across repeated Ack calls.
func TestDelivery_AckIdempotentSuccess(t *testing.T) {
	var calls int
	del := NewDelivery(
		messaging.MustEnvelope(messaging.EnvelopeInput{}),
		WithAckFunc(func() error { calls++; return nil }),
	)
	for i := 0; i < 3; i++ {
		if err := del.Ack(context.Background()); err != nil {
			t.Fatalf("Ack #%d = %v, want nil", i, err)
		}
	}
	if calls != 1 {
		t.Fatalf("ack callback invoked %d times, want 1", calls)
	}
}

// verifies Retry returns ErrNotSupported.
func TestDelivery_RetryNotSupported(t *testing.T) {
	del := NewDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{}))
	err := del.Retry(context.Background(), time.Second, errors.New("reason"))
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Errorf("Retry() = %v, want ErrNotSupported", err)
	}
}

// verifies Extend returns ErrNotSupported.
func TestDelivery_ExtendNotSupported(t *testing.T) {
	del := NewDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{}))
	err := del.Extend(context.Background(), time.Now().Add(time.Minute))
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Errorf("Extend() = %v, want ErrNotSupported", err)
	}
}

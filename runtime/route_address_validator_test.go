package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// recordingAddressValidator records each ValidateAddress call and
// returns the configured error verbatim (nil ⇒ pass).
type recordingAddressValidator struct {
	err  error
	hits int
}

func (r *recordingAddressValidator) ValidateAddress(string) error {
	r.hits++
	return r.err
}

// TestRouteRunner_AddressValidator_Reject_RoutesDLQ covers
// generic dispatch path: a non-nil AddressValidator that returns an
// error must short-circuit the send and route the message to the DLQ
// with shared.ErrInvalidTopic semantics.
func TestRouteRunner_AddressValidator_Reject_RoutesDLQ(t *testing.T) {
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()
	validator := &recordingAddressValidator{err: errors.New("nope")}

	bindings := []routing.DestinationBinding{
		{ID: "b1", Transport: "fake", Address: "subject/static"},
	}

	receiver := NewFakeReceiver()
	cfg := route.RouteRunnerConfig{
		RouteID:           "validator-rejects",
		Policy:            routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver:          receiver,
		Sender:            sender,
		Bindings:          bindings,
		AddressValidators: map[string]ports.AddressValidator{"b1": validator},
		DLQ:               dlq.New(dlqStore),
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Subject: "test"})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	assert.Equal(t, 1, validator.hits,
		"validator must be invoked exactly once for the rendered address")
	assert.Equal(t, 0, sender.SentCount(),
		"sender must not be called when the validator rejects")
	assert.Equal(t, 1, dlqStore.Count(),
		"rejected address must produce one DLQ entry")
	if dlqStore.Count() == 1 {
		entry := dlqStore.Entries[0]
		// maps validator failure to ErrInvalidTopic; the DLQ
		// entry's ErrorCode must therefore advertise that.
		if entry.ErrorCode() != string(shared.ErrInvalidTopic.Code) {
			t.Fatalf("expected DLQ ErrorCode %q, got %q",
				shared.ErrInvalidTopic.Code, entry.ErrorCode())
		}
	}
}

// TestRouteRunner_AddressValidator_NilSkipsValidation guards the
// "transport opted out of address validation" case: a binding whose
// transport returned nil from AddressValidator must be dispatched
// without any validator running, and the sender must receive the
// unchanged address.
func TestRouteRunner_AddressValidator_NilSkipsValidation(t *testing.T) {
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	bindings := []routing.DestinationBinding{
		{ID: "b1", Transport: "fake", Address: "subject/static"},
	}

	receiver := NewFakeReceiver()
	cfg := route.RouteRunnerConfig{
		RouteID:  "no-validator",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		Bindings: bindings,
		// AddressValidators intentionally nil — transport opted out.
		DLQ: dlq.New(dlqStore),
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-2", Subject: "test"})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	assert.Equal(t, 1, sender.SentCount(),
		"sender must be invoked when no validator is registered")
	assert.Equal(t, 0, dlqStore.Count(),
		"absent validator must not produce a DLQ entry")
}

// (removed errorContains helper — no current callers)

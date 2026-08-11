package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// rejectingAddressValidator implements ports.AddressValidator for tests
// that exercise the per-binding validator dispatch path. Every
// call returns an error so the runtime maps it to ErrInvalidTopic.
type rejectingAddressValidator struct{}

func (rejectingAddressValidator) ValidateAddress(string) error {
	return errors.New("address rejected by test validator")
}

func TestResolvePlans_NoResolver_RendersAddress(t *testing.T) {
	sender := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{ID: "b1", Address: "devices/{tenant}/events"},
	}

	receiver := NewFakeReceiver()
	cfg := route.RouteRunnerConfig{
		RouteID:  "fallback-render",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(NewFakeDLQStore()),
		Bindings: bindings,
		// No Resolver — exercises the fallback path.
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "render-fallback",
		Subject: "test",
		Headers: map[string]any{"tenant": "acme"},
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	sent := sender.GetSent()
	require.Len(t, sent, 1)
	assert.Equal(t, "test", sent[0].Subject(),
		"source logical Subject must be preserved on the outbound envelope")
	out := sender.GetOutbound()
	require.Len(t, out, 1)
	assert.Equal(t, "devices/acme/events", out[0].Address,
		"fallback path should render address template into OutboundMessage.Address")
}

func TestResolvePlans_NoResolver_RenderError_RoutesDLQ(t *testing.T) {
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	bindings := []routing.DestinationBinding{
		{ID: "b1", Address: "devices/{tenant}/events"},
	}

	receiver := NewFakeReceiver()
	cfg := route.RouteRunnerConfig{
		RouteID:  "fallback-render-err",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(dlqStore),
		Bindings: bindings,
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// No "tenant" header -> RenderAddress fails.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "render-fallback-err", Subject: "test"})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	assert.Equal(t, 0, sender.SentCount(),
		"sender must not be called when address render fails")
	assert.Equal(t, 1, dlqStore.Count(),
		"expected 1 DLQ entry for render error")
}

func TestResolvePlans_NoResolver_CopiesBindingHeaders(t *testing.T) {
	sender := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{
			ID:      "b1",
			Address: "topic/out",
			Headers: map[string]any{"qos": 1, "retain": true},
		},
	}

	receiver := NewFakeReceiver()
	cfg := route.RouteRunnerConfig{
		RouteID:  "fallback-options",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(NewFakeDLQStore()),
		Bindings: bindings,
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "options-msg", Subject: "test"})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	sent := sender.GetSent()
	require.Len(t, sent, 1)
	assert.Equal(t, 1, sent[0].Headers()["qos"],
		"binding Headers should be merged into envelope headers")
	assert.Equal(t, true, sent[0].Headers()["retain"],
		"binding Headers should be merged into envelope headers")
}

func TestResolvePlans_NoResolver_AddressValidatorRejects(t *testing.T) {
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	bindings := []routing.DestinationBinding{
		{ID: "mqtt-b", Transport: "mqtt", Address: "devices/{topic}/events"},
	}

	receiver := NewFakeReceiver()
	cfg := route.RouteRunnerConfig{
		RouteID:  "fallback-validate",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(dlqStore),
		Bindings: bindings,
		// validation is now a transport-supplied capability
		// dispatched per binding by the runtime. The test wires a
		// rejecting validator to assert the fallback path enforces it.
		AddressValidators: map[string]ports.AddressValidator{
			"mqtt-b": rejectingAddressValidator{},
		},
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "validator-fallback-bad",
		Subject: "test",
		Headers: map[string]any{"topic": "factory/#"},
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	assert.Equal(t, 0, sender.SentCount(),
		"sender must not be called when AddressValidator rejects")
	assert.Equal(t, 1, dlqStore.Count(),
		"expected 1 DLQ entry for AddressValidator rejection")
}

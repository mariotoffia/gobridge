package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/runtime"
)

func TestResolvePlans_NoResolver_RendersAddress(t *testing.T) {
	sender := NewFakeSender()

	bindings := []domain.DestinationBinding{
		{ID: "b1", Address: "devices/{tenant}/events"},
	}

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "fallback-render",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      runtime.NewDLQRouter(NewFakeDLQStore()),
		Bindings: bindings,
		// No Resolver — exercises the fallback path.
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := &domain.Envelope{
		ID:      "render-fallback",
		Subject: "test",
		Headers: map[string]any{"tenant": "acme"},
	}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	sent := sender.GetSent()
	require.Len(t, sent, 1)
	assert.Equal(t, "devices/acme/events", sent[0].Subject,
		"fallback path should render address template")
}

func TestResolvePlans_NoResolver_RenderError_RoutesDLQ(t *testing.T) {
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	bindings := []domain.DestinationBinding{
		{ID: "b1", Address: "devices/{tenant}/events"},
	}

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "fallback-render-err",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      runtime.NewDLQRouter(dlqStore),
		Bindings: bindings,
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// No "tenant" header -> RenderAddress fails.
	env := &domain.Envelope{ID: "render-fallback-err", Subject: "test"}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	assert.Equal(t, 0, sender.SentCount(),
		"sender must not be called when address render fails")
	assert.Equal(t, 1, dlqStore.Count(),
		"expected 1 DLQ entry for render error")
}

func TestResolvePlans_NoResolver_CopiesBindingOptions(t *testing.T) {
	sender := NewFakeSender()

	bindings := []domain.DestinationBinding{
		{
			ID:      "b1",
			Address: "topic/out",
			Options: map[string]any{"qos": 1, "retain": true},
		},
	}

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "fallback-options",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      runtime.NewDLQRouter(NewFakeDLQStore()),
		Bindings: bindings,
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := &domain.Envelope{ID: "options-msg", Subject: "test"}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	sent := sender.GetSent()
	require.Len(t, sent, 1)
	assert.Equal(t, 1, sent[0].Headers["qos"],
		"binding Options should be merged into envelope headers")
	assert.Equal(t, true, sent[0].Headers["retain"],
		"binding Options should be merged into envelope headers")
}

func TestResolvePlans_NoResolver_MQTTValidation(t *testing.T) {
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	bindings := []domain.DestinationBinding{
		{ID: "mqtt-b", Transport: "mqtt", Address: "devices/{topic}/events"},
	}

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "fallback-mqtt-validate",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      runtime.NewDLQRouter(dlqStore),
		Bindings: bindings,
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Header produces invalid MQTT topic with wildcard.
	env := &domain.Envelope{
		ID:      "mqtt-fallback-bad",
		Subject: "test",
		Headers: map[string]any{"topic": "factory/#"},
	}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	assert.Equal(t, 0, sender.SentCount(),
		"sender must not be called for invalid MQTT topic")
	assert.Equal(t, 1, dlqStore.Count(),
		"expected 1 DLQ entry for MQTT validation")
}

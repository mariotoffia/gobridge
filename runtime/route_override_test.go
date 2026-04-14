package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// overrideProcessor sets HeaderRouteOverride during the processor chain,
// simulating what filter.ActionRoute does in production.
type overrideProcessor struct {
	targetBinding string
}

func (p *overrideProcessor) Name() string { return "test-override" }

func (p *overrideProcessor) Process(_ context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	env.Headers = domain.SetHeader(env.Headers, domain.HeaderRouteOverride, p.targetBinding)
	return next(context.Background(), env)
}

// ---------------------------------------------------------------------------
// TestDirectHold_HeaderRouteOverride_SelectsBinding
// ---------------------------------------------------------------------------

func TestDirectHold_HeaderRouteOverride_SelectsBinding(t *testing.T) {
	senderA := NewFakeSender()
	senderB := NewFakeSender()

	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Address: "topic-a"},
		{ID: "bind-b", Address: "topic-b"},
	}

	// Resolver that would normally select bind-a for everything.
	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bind-a"}, // no conditions = always match
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "")

	// The override is set by a PROCESSOR (simulating filter ActionRoute),
	// not in the initial envelope — because reserved headers are stripped
	// at ingress before the processor chain runs.
	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:    "override-route",
		Policy:     domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold}.WithDefaults(),
		Receiver:   receiver,
		Sender:     senderA,
		Senders:    map[string]ports.Sender{"bind-a": senderA, "bind-b": senderB},
		DLQ:        runtime.NewDLQRouter(NewFakeDLQStore()),
		Resolver:   resolver,
		Bindings:   bindings,
		Processors: []ports.Processor{&overrideProcessor{targetBinding: "bind-b"}},
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := &domain.Envelope{ID: "override-msg", Subject: "test"}
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	// Override processor forced routing to bind-b → senderB.
	if len(senderB.GetSent()) != 1 {
		t.Fatalf("senderB: expected 1, got %d", len(senderB.GetSent()))
	}
	if len(senderA.GetSent()) != 0 {
		t.Fatalf("senderA: expected 0, got %d", len(senderA.GetSent()))
	}
}

// ---------------------------------------------------------------------------
// TestDirectHold_HeaderRouteOverride_StrippedAfterUse
// ---------------------------------------------------------------------------

func TestDirectHold_HeaderRouteOverride_StrippedAfterUse(t *testing.T) {
	senderB := NewFakeSender()

	bindings := []domain.DestinationBinding{
		{ID: "bind-b", Address: "topic-b"},
	}

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:    "strip-route",
		Policy:     domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold}.WithDefaults(),
		Receiver:   receiver,
		Sender:     senderB,
		Senders:    map[string]ports.Sender{"bind-b": senderB},
		DLQ:        runtime.NewDLQRouter(NewFakeDLQStore()),
		Bindings:   bindings,
		Processors: []ports.Processor{&overrideProcessor{targetBinding: "bind-b"}},
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := &domain.Envelope{ID: "strip-msg", Subject: "test"}
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	sent := senderB.GetSent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent, got %d", len(sent))
	}
	// The override header should have been stripped before sending.
	if _, exists := sent[0].Headers[domain.HeaderRouteOverride]; exists {
		t.Fatal("HeaderRouteOverride was not stripped from outbound envelope")
	}
}

// ---------------------------------------------------------------------------
// TestDirectHold_HeaderRouteOverride_InvalidBinding_FallsThrough
// ---------------------------------------------------------------------------

func TestDirectHold_HeaderRouteOverride_InvalidBinding_FallsThrough(t *testing.T) {
	defaultSender := NewFakeSender()

	bindings := []domain.DestinationBinding{
		{ID: "real-bind", Address: "topic"},
	}

	receiver, runner := makeRunnerWithSenders(t, bindings, nil, nil, defaultSender)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Override references a binding that doesn't exist on this route.
	env := &domain.Envelope{
		ID:      "bad-override",
		Subject: "test",
		Headers: map[string]any{
			domain.HeaderRouteOverride: "nonexistent-bind",
		},
	}
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	// Should fall through to normal resolution (first binding via default sender).
	if len(defaultSender.GetSent()) != 1 {
		t.Fatalf("expected fallthrough to default sender, got %d sent", len(defaultSender.GetSent()))
	}
}

// ---------------------------------------------------------------------------
// TestDirectHold_Override_RenderAddressError_RoutesDLQ
// ---------------------------------------------------------------------------

func TestDirectHold_Override_RenderAddressError_RoutesDLQ(t *testing.T) {
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	bindings := []domain.DestinationBinding{
		{ID: "bind-template", Address: "devices/{tenant}/events"},
	}

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "render-err-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		Senders:  map[string]ports.Sender{"bind-template": sender},
		DLQ:      runtime.NewDLQRouter(dlqStore),
		Bindings: bindings,
		Processors: []ports.Processor{
			&overrideProcessor{targetBinding: "bind-template"},
		},
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Envelope has no "tenant" header -> RenderAddress fails.
	env := &domain.Envelope{ID: "missing-header-msg", Subject: "test"}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	// Must go to DLQ, not be sent with raw template address.
	assert.Equal(t, 0, sender.SentCount(),
		"sender must not be called when address template fails")
	assert.Equal(t, 1, dlqStore.Count(),
		"expected 1 DLQ entry for address template error")
}

// ---------------------------------------------------------------------------
// TestDirectHold_Override_MQTTValidation_RoutesDLQ
// ---------------------------------------------------------------------------

func TestDirectHold_Override_MQTTValidation_RoutesDLQ(t *testing.T) {
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	bindings := []domain.DestinationBinding{
		{ID: "mqtt-bind", Transport: "mqtt", Address: "devices/{topic}/events"},
	}

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "mqtt-validate-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		Senders:  map[string]ports.Sender{"mqtt-bind": sender},
		DLQ:      runtime.NewDLQRouter(dlqStore),
		Bindings: bindings,
		Processors: []ports.Processor{
			&overrideProcessor{targetBinding: "mqtt-bind"},
		},
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Header value produces invalid MQTT topic (contains wildcard).
	env := &domain.Envelope{
		ID:      "bad-mqtt-msg",
		Subject: "test",
		Headers: map[string]any{"topic": "factory/+/data"},
	}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	assert.Equal(t, 0, sender.SentCount(),
		"sender must not be called for invalid MQTT topic")
	assert.Equal(t, 1, dlqStore.Count(),
		"expected 1 DLQ entry for invalid MQTT topic")
}

// ---------------------------------------------------------------------------
// TestDirectHold_NoOverrideHeader_NormalResolution
// ---------------------------------------------------------------------------

func TestDirectHold_NoOverrideHeader_NormalResolution(t *testing.T) {
	senderA := NewFakeSender()

	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Address: "topic-a"},
	}

	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bind-a"},
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "")

	receiver, runner := makeRunnerWithSenders(t, bindings, resolver,
		map[string]ports.Sender{"bind-a": senderA}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// No override header — normal resolution path.
	env := &domain.Envelope{ID: "normal-msg", Subject: "test"}
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	if len(senderA.GetSent()) != 1 {
		t.Fatalf("expected 1 sent, got %d", len(senderA.GetSent()))
	}
}

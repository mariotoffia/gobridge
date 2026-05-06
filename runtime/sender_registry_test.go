package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// makeRunnerWithSenders creates a RouteRunner with a resolver and per-binding
// sender registry for testing content-based DirectHold dispatch.
func makeRunnerWithSenders(
	t *testing.T,
	bindings []routing.DestinationBinding,
	resolver ports.DestinationResolver,
	senders map[string]ports.Sender,
	defaultSender *FakeSender,
) (*FakeReceiver, *runtime.RouteRunner) {
	t.Helper()
	receiver := NewFakeReceiver()
	if defaultSender == nil {
		defaultSender = NewFakeSender()
	}

	cfg := runtime.RouteRunnerConfig{
		RouteID:  "test-multi-sender",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   defaultSender,
		Senders:  senders,
		DLQ:      runtime.NewDLQRouter(NewFakeDLQStore()),
		Resolver: resolver,
		Bindings: bindings,
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)
	return receiver, runner
}

// ---------------------------------------------------------------------------
// TestDirectHold_SenderRegistry_SelectsByBinding
// ---------------------------------------------------------------------------

func TestDirectHold_SenderRegistry_SelectsByBinding(t *testing.T) {
	senderA := NewFakeSender()
	senderB := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Address: "topic-a"},
		{ID: "bind-b", Address: "topic-b"},
	}

	rules, err := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bind-a", Conditions: []runtime.MatchCondition{
			{Field: "header.target", Operator: "eq", Value: "a"},
		}},
		{BindingID: "bind-b", Conditions: []runtime.MatchCondition{
			{Field: "header.target", Operator: "eq", Value: "b"},
		}},
	})
	if err != nil {
		t.Fatalf("CompileMatchRules: %v", err)
	}
	resolver, err := runtime.NewRuleResolver(bindings, rules, "")
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}

	receiver, runner := makeRunnerWithSenders(t, bindings, resolver,
		map[string]ports.Sender{"bind-a": senderA, "bind-b": senderB}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Send message targeting binding A.
	envA := &messaging.Envelope{
		ID:      "msg-a",
		Subject: "test",
		Headers: map[string]any{"target": "a"},
	}
	delA := NewFakeDelivery(envA)
	if err := receiver.Emit(ctx, delA); err != nil {
		t.Fatalf("Emit A: %v", err)
	}
	waitFor(t, 2*time.Second, "delivery A acked", delA.IsAcked)

	// Send message targeting binding B.
	envB := &messaging.Envelope{
		ID:      "msg-b",
		Subject: "test",
		Headers: map[string]any{"target": "b"},
	}
	delB := NewFakeDelivery(envB)
	if err := receiver.Emit(ctx, delB); err != nil {
		t.Fatalf("Emit B: %v", err)
	}
	waitFor(t, 2*time.Second, "delivery B acked", delB.IsAcked)

	// Verify each sender received the correct message.
	sentA := senderA.GetSent()
	if len(sentA) != 1 {
		t.Fatalf("senderA: expected 1 message, got %d", len(sentA))
	}
	if sentA[0].ID != "msg-a" {
		t.Fatalf("senderA: expected msg-a, got %s", sentA[0].ID)
	}

	sentB := senderB.GetSent()
	if len(sentB) != 1 {
		t.Fatalf("senderB: expected 1 message, got %d", len(sentB))
	}
	if sentB[0].ID != "msg-b" {
		t.Fatalf("senderB: expected msg-b, got %s", sentB[0].ID)
	}
}

// ---------------------------------------------------------------------------
// TestDirectHold_SenderRegistry_FallsBackToDefault
// ---------------------------------------------------------------------------

func TestDirectHold_SenderRegistry_FallsBackToDefault(t *testing.T) {
	specificSender := NewFakeSender()
	defaultSender := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{ID: "specific", Address: "specific-topic"},
		{ID: "fallback", Address: "fallback-topic"},
	}

	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "specific", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "eq", Value: "special"},
		}},
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "fallback")

	// Only register a sender for "specific"; "fallback" uses the default.
	receiver, runner := makeRunnerWithSenders(t, bindings, resolver,
		map[string]ports.Sender{"specific": specificSender}, defaultSender)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Send a message that matches no rule → falls through to default binding.
	env := &messaging.Envelope{ID: "msg-fb", Subject: "normal"}
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	// Default sender should have received it (fallback binding uses default sender).
	sentDefault := defaultSender.GetSent()
	if len(sentDefault) != 1 {
		t.Fatalf("defaultSender: expected 1, got %d", len(sentDefault))
	}

	sentSpecific := specificSender.GetSent()
	if len(sentSpecific) != 0 {
		t.Fatalf("specificSender: expected 0, got %d", len(sentSpecific))
	}
}

// ---------------------------------------------------------------------------
// TestDirectHold_SenderRegistry_NilRegistry_UsesDefault
// ---------------------------------------------------------------------------

func TestDirectHold_SenderRegistry_NilRegistry_UsesDefault(t *testing.T) {
	defaultSender := NewFakeSender()
	bindings := []routing.DestinationBinding{{ID: "only", Address: "topic"}}

	// No sender registry, no resolver → uses default sender for first binding.
	receiver, runner := makeRunnerWithSenders(t, bindings, nil, nil, defaultSender)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := &messaging.Envelope{ID: "msg-1", Subject: "test"}
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	sent := defaultSender.GetSent()
	if len(sent) != 1 {
		t.Fatalf("expected 1, got %d", len(sent))
	}
}

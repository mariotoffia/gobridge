package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// ---------------------------------------------------------------------------
// Integration: Header-based routing to different senders (DirectHold)
// ---------------------------------------------------------------------------

func TestIntegration_ContentRouting_HeaderMatch_DirectHold(t *testing.T) {
	senderOrders := NewFakeSender()
	senderAlerts := NewFakeSender()
	senderDefault := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{ID: "bind-orders", Address: "topic-orders"},
		{ID: "bind-alerts", Address: "topic-alerts"},
		{ID: "bind-default", Address: "topic-default"},
	}

	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bind-orders", Conditions: []runtime.MatchCondition{
			{Field: "header.category", Operator: "eq", Value: runtime.Val("order")},
		}},
		{BindingID: "bind-alerts", Conditions: []runtime.MatchCondition{
			{Field: "header.category", Operator: "eq", Value: runtime.Val("alert")},
		}},
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "bind-default")

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "header-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   senderDefault,
		Senders: map[string]ports.Sender{
			"bind-orders":  senderOrders,
			"bind-alerts":  senderAlerts,
			"bind-default": senderDefault,
		},
		DLQ:      dlq.New(NewFakeDLQStore()),
		Resolver: resolver,
		Bindings: bindings,
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Send three messages with different categories.
	msgs := []struct {
		id       string
		category string
	}{
		{"msg-1", "order"},
		{"msg-2", "alert"},
		{"msg-3", "unknown"},
	}
	for _, m := range msgs {
		del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      m.id,
			Subject: "test",
			Headers: map[string]any{"category": m.category},
		}))
		if err := receiver.Emit(ctx, del); err != nil {
			t.Fatalf("Emit %s: %v", m.id, err)
		}
		waitFor(t, 2*time.Second, m.id+" acked", del.IsAcked)
	}

	if len(senderOrders.GetSent()) != 1 {
		t.Fatalf("orders sender: expected 1, got %d", len(senderOrders.GetSent()))
	}
	if len(senderAlerts.GetSent()) != 1 {
		t.Fatalf("alerts sender: expected 1, got %d", len(senderAlerts.GetSent()))
	}
	if len(senderDefault.GetSent()) != 1 {
		t.Fatalf("default sender: expected 1, got %d", len(senderDefault.GetSent()))
	}
}

// ---------------------------------------------------------------------------
// Integration: Subject-prefix routing
// ---------------------------------------------------------------------------

func TestIntegration_ContentRouting_SubjectPrefix_DirectHold(t *testing.T) {
	senderEU := NewFakeSender()
	senderUS := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{ID: "bind-eu", Address: "eu-events"},
		{ID: "bind-us", Address: "us-events"},
	}

	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bind-eu", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "prefix", Value: runtime.Val("eu.")},
		}},
		{BindingID: "bind-us", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "prefix", Value: runtime.Val("us.")},
		}},
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "")

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "subject-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   senderEU,
		Senders:  map[string]ports.Sender{"bind-eu": senderEU, "bind-us": senderUS},
		DLQ:      dlq.New(NewFakeDLQStore()),
		Resolver: resolver,
		Bindings: bindings,
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "eu-1", Subject: "eu.orders.new"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "eu acked", del.IsAcked)

	if len(senderEU.GetSent()) != 1 || senderEU.GetSent()[0].ID != "eu-1" {
		t.Fatal("EU sender should have received eu-1")
	}
	if len(senderUS.GetSent()) != 0 {
		t.Fatal("US sender should have received nothing")
	}
}

// ---------------------------------------------------------------------------
// Integration: JSON payload routing
// ---------------------------------------------------------------------------

func TestIntegration_ContentRouting_JSONPayload_DirectHold(t *testing.T) {
	senderHigh := NewFakeSender()
	senderLow := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{ID: "high-prio", Address: "high-queue"},
		{ID: "low-prio", Address: "low-queue"},
	}

	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "high-prio", Conditions: []runtime.MatchCondition{
			{Field: "$.priority", Operator: "gt", Value: runtime.Val(float64(7))},
		}},
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "low-prio")

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "json-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   senderLow,
		Senders:  map[string]ports.Sender{"high-prio": senderHigh, "low-prio": senderLow},
		DLQ:      dlq.New(NewFakeDLQStore()),
		Resolver: resolver,
		Bindings: bindings,
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// High priority message
	delH := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "high-1", Subject: "evt", Payload: []byte(`{"priority":9}`),
	}))
	_ = receiver.Emit(ctx, delH)
	waitFor(t, 2*time.Second, "high acked", delH.IsAcked)

	// Low priority message
	delL := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "low-1", Subject: "evt", Payload: []byte(`{"priority":3}`),
	}))
	_ = receiver.Emit(ctx, delL)
	waitFor(t, 2*time.Second, "low acked", delL.IsAcked)

	if len(senderHigh.GetSent()) != 1 {
		t.Fatalf("high sender: expected 1, got %d", len(senderHigh.GetSent()))
	}
	if len(senderLow.GetSent()) != 1 {
		t.Fatalf("low sender: expected 1, got %d", len(senderLow.GetSent()))
	}
}

// ---------------------------------------------------------------------------
// Integration: Processor chain + content routing (transform then route)
// ---------------------------------------------------------------------------

func TestIntegration_ContentRouting_ProcessorThenRouting(t *testing.T) {
	senderVIP := NewFakeSender()
	senderNormal := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{ID: "bind-vip", Address: "vip-stream"},
		{ID: "bind-normal", Address: "normal-stream"},
	}

	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bind-vip", Conditions: []runtime.MatchCondition{
			{Field: "header.tier", Operator: "eq", Value: runtime.Val("vip")},
		}},
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "bind-normal")

	// Processor that sets tier=vip when subject starts with "premium."
	tierProc := &headerInjector{
		matchFn:   func(env *messaging.Envelope) bool { return len(env.Subject()) > 8 && env.Subject()[:8] == "premium." },
		headerKey: "tier",
		headerVal: "vip",
	}

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:    "proc-route",
		Policy:     routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver:   receiver,
		Sender:     senderNormal,
		Senders:    map[string]ports.Sender{"bind-vip": senderVIP, "bind-normal": senderNormal},
		DLQ:        dlq.New(NewFakeDLQStore()),
		Resolver:   resolver,
		Bindings:   bindings,
		Processors: []ports.Processor{tierProc},
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "vip-msg", Subject: "premium.order"}))
	_ = receiver.Emit(ctx, del)
	waitFor(t, 2*time.Second, "vip acked", del.IsAcked)

	if len(senderVIP.GetSent()) != 1 {
		t.Fatalf("VIP sender: expected 1, got %d", len(senderVIP.GetSent()))
	}
	if len(senderNormal.GetSent()) != 0 {
		t.Fatalf("normal sender: expected 0, got %d", len(senderNormal.GetSent()))
	}
}

// headerInjector is a test processor that injects a header when matchFn returns true.
type headerInjector struct {
	matchFn   func(*messaging.Envelope) bool
	headerKey string
	headerVal string
}

func (p *headerInjector) Name() string { return "test-header-injector" }

func (p *headerInjector) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	if p.matchFn(env) {
		env.SetHeader(p.headerKey, p.headerVal)
	}
	return next(ctx, env)
}

// ---------------------------------------------------------------------------
// Integration: Concurrent content routing (race detection)
// ---------------------------------------------------------------------------

func TestIntegration_ContentRouting_Concurrent(t *testing.T) {
	senderA := NewFakeSender()
	senderB := NewFakeSender()

	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Address: "topic-a"},
		{ID: "bind-b", Address: "topic-b"},
	}

	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bind-a", Conditions: []runtime.MatchCondition{
			{Field: "header.target", Operator: "eq", Value: runtime.Val("a")},
		}},
		{BindingID: "bind-b", Conditions: []runtime.MatchCondition{
			{Field: "header.target", Operator: "eq", Value: runtime.Val("b")},
		}},
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "")

	receiver := NewFakeReceiver()
	cfg := runtime.RouteRunnerConfig{
		RouteID:  "concurrent-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 20}.WithDefaults(),
		Receiver: receiver,
		Sender:   senderA,
		Senders:  map[string]ports.Sender{"bind-a": senderA, "bind-b": senderB},
		DLQ:      dlq.New(NewFakeDLQStore()),
		Resolver: resolver,
		Bindings: bindings,
	}
	runner := runtime.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			target := "a"
			if idx%2 == 1 {
				target = "b"
			}
			del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "msg-" + target + "-" + string(rune('0'+idx%10)),
				Subject: "test",
				Headers: map[string]any{"target": target},
			}))
			_ = receiver.Emit(ctx, del)
			waitFor(t, 5*time.Second, "concurrent msg acked", del.IsAcked)
		}(i)
	}
	wg.Wait()

	totalA := len(senderA.GetSent())
	totalB := len(senderB.GetSent())
	if totalA+totalB != n {
		t.Fatalf("expected %d total, got %d (A=%d, B=%d)", n, totalA+totalB, totalA, totalB)
	}
	if totalA != 25 || totalB != 25 {
		t.Fatalf("expected 25/25 split, got A=%d B=%d", totalA, totalB)
	}
}

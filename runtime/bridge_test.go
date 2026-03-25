package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

func TestRuntime_StartStop(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-test"),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID:     "route-1",
		Policy: domain.RoutePolicy{}.WithDefaults(),
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	if err := rt.AddRoute(cfg, receiver, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute failed: %v", err)
	}

	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := rt.Stop(stopCtx); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

func TestRuntime_DuplicateRoute(t *testing.T) {
	rt := goruntime.New()
	cfg := goruntime.RouteConfig{ID: "dup"}
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	err := rt.AddRoute(cfg, receiver, sender, nil, nil)
	if err == nil {
		t.Fatal("expected duplicate route error")
	}
}

func TestRuntime_AddRouteWhileRunning(t *testing.T) {
	rt := goruntime.New()
	cfg := goruntime.RouteConfig{
		ID:                 "r1",
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	_ = rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	err := rt.AddRoute(goruntime.RouteConfig{ID: "r2"}, NewFakeReceiver(), NewFakeSender(), nil, nil)
	if err == nil {
		t.Fatal("should not add route while running")
	}
}

func TestRuntime_DirectHoldEndToEnd(t *testing.T) {
	dlqStore := NewFakeDLQStore()
	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-e2e"),
		goruntime.WithDLQStore(dlqStore),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "e2e-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	ctx := context.Background()
	_ = rt.Start(ctx)

	env := &domain.Envelope{ID: "e2e-msg", Payload: []byte("hello")}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	time.Sleep(50 * time.Millisecond)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if !del.Acked {
		t.Fatal("delivery should be acked")
	}

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = rt.Stop(stopCtx)
}

func TestRuntime_Inject_HappyPath(t *testing.T) {
	dlqStore := NewFakeDLQStore()
	rt := goruntime.New(
		goruntime.WithInstanceID("inject-test"),
		goruntime.WithDLQStore(dlqStore),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "inject-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	ctx := context.Background()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	time.Sleep(50 * time.Millisecond)

	env := &domain.Envelope{
		ID:      "injected-1",
		Subject: "test/inject",
		Payload: []byte(`{"injected":true}`),
		Headers: map[string]any{"custom": "value"},
	}
	if err := rt.Inject(ctx, "inject-route", env); err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent message from injection, got %d", sender.SentCount())
	}
	sent := sender.Sent[0]
	if string(sent.Payload) != `{"injected":true}` {
		t.Fatalf("unexpected payload: %s", sent.Payload)
	}
}

func TestRuntime_Inject_UnknownRoute(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("inject-unknown"))

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID:                 "existing-route",
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	time.Sleep(50 * time.Millisecond)

	err := rt.Inject(context.Background(), "nonexistent", &domain.Envelope{ID: "x"})
	if err == nil {
		t.Fatal("expected error for unknown route")
	}
}

func TestRuntime_Inject_NotRunning(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("inject-stopped"))

	err := rt.Inject(context.Background(), "any-route", &domain.Envelope{ID: "x"})
	if err == nil {
		t.Fatal("expected error when runtime is not running")
	}
}

func TestRuntime_Inject_AssignsIDWhenEmpty(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("inject-id"))

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "id-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	time.Sleep(50 * time.Millisecond)

	env := &domain.Envelope{Payload: []byte("no-id")}
	if err := rt.Inject(context.Background(), "id-route", env); err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if sender.Sent[0].ID == "" {
		t.Fatal("injected envelope should have an auto-assigned ID")
	}
	if env.ID != "" {
		t.Fatal("original envelope should not be mutated (clone)")
	}
}

func TestRuntime_Inject_DoesNotMutateOriginal(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("inject-clone"))

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "clone-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	time.Sleep(50 * time.Millisecond)

	env := &domain.Envelope{
		ID:      "orig-id",
		Headers: map[string]any{"keep": "this"},
	}
	_ = rt.Inject(context.Background(), "clone-route", env)

	time.Sleep(50 * time.Millisecond)

	if len(env.Headers) != 1 {
		t.Fatalf("original headers should not be modified, got %d entries", len(env.Headers))
	}
	if env.Headers["keep"] != "this" {
		t.Fatal("original header value should be preserved")
	}
}

func TestRuntime_SharedOutboxEndToEnd(t *testing.T) {
	dlqStore := NewFakeDLQStore()
	outbox := NewFakeOutboxStore()
	leaseStore := NewFakeLeaseStore()

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-outbox"),
		goruntime.WithDLQStore(dlqStore),
		goruntime.WithOutboxStore(outbox),
		goruntime.WithLeaseStore(leaseStore),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	session := NewFakeSession()

	sessCfg := goruntime.DefaultSessionConfig("sess-e2e", true)
	sessCfg.LeaseTTL = 500 * time.Millisecond
	sessCfg.RenewInterval = 100 * time.Millisecond
	sessCfg.DrainStrategy = domain.NewFixedPoll(50 * time.Millisecond)

	cfg := goruntime.RouteConfig{
		ID: "outbox-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []domain.DispatchPlan{
				{BindingID: "bind-e2e", Address: "topic/e2e"},
			},
		},
		Bindings: []domain.DestinationBinding{
			{ID: "bind-e2e", SessionID: "sess-e2e"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, session, &sessCfg)
	ctx := context.Background()
	_ = rt.Start(ctx)

	time.Sleep(200 * time.Millisecond)

	env := &domain.Envelope{ID: "outbox-msg", Payload: []byte("outbox-data")}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	time.Sleep(100 * time.Millisecond)

	if !del.Acked {
		t.Fatal("delivery should be acked after outbox persist")
	}
	if outbox.RecordCount() == 0 {
		t.Fatal("expected outbox records")
	}

	time.Sleep(500 * time.Millisecond)

	if sender.SentCount() < 1 {
		t.Fatalf("expected at least 1 sent from outbox drain, got %d", sender.SentCount())
	}

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = rt.Stop(stopCtx)
}

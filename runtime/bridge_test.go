package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// TestRuntime_WithClock_NilIgnored regresses the contract that
// WithClock(nil) must not overwrite the default clock with nil — otherwise
// every subsequent ticker/timer dereference panics. The fix is in
// runtime/bridge.go's WithClock option.
func TestRuntime_WithClock_NilIgnored(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("nil-clock-test"),
		goruntime.WithClock(nil),
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start with nil clock option: %v", err)
	}
	if err := rt.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Verifies that the runtime starts and stops cleanly with a single route.
func TestRuntime_StartStop(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-test"),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "route-1",
		Policy: routing.RoutePolicy{
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		}.WithDefaults(),
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}

	if err := rt.AddRoute(cfg, receiver, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute failed: %v", err)
	}

	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	<-receiver.Ready()

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := rt.Stop(stopCtx); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

// Verifies that adding a route with the same ID as an existing route fails.
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

// Verifies that routes cannot be added while the runtime is running.
func TestRuntime_AddRouteWhileRunning(t *testing.T) {
	rt := goruntime.New()
	cfg := goruntime.RouteConfig{
		ID: "r1",
		Policy: routing.RoutePolicy{
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}

	_ = rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	err := rt.AddRoute(goruntime.RouteConfig{ID: "r2"}, NewFakeReceiver(), NewFakeSender(), nil, nil)
	if err == nil {
		t.Fatal("should not add route while running")
	}
}

// Verifies direct-hold delivery: receiver message is forwarded, delivery acked, sender sees one send.
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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	ctx := context.Background()
	_ = rt.Start(ctx)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e2e-msg", Payload: []byte("hello")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "e2e send and ack", func() bool {
		return del.IsAcked() && sender.SentCount() == 1
	})

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if !del.IsAcked() {
		t.Fatal("delivery should be acked")
	}

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = rt.Stop(stopCtx)
}

// Verifies Inject delivers an envelope through a running route to the sender with expected payload.
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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}

	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	ctx := context.Background()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "injected-1",
		Subject: "test/inject",
		Payload: []byte(`{"injected":true}`),
		Headers: map[string]any{"custom": "value"},
	})
	if err := rt.Inject(ctx, "inject-route", env); err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	waitFor(t, time.Second, "inject processed and sent", func() bool {
		return sender.SentCount() == 1
	})

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent message from injection, got %d", sender.SentCount())
	}
	sent := sender.GetSent()[0]
	if string(sent.Payload()) != `{"injected":true}` {
		t.Fatalf("unexpected payload: %s", sent.Payload())
	}
}

// Verifies Inject returns an error when the route ID does not exist.
func TestRuntime_Inject_UnknownRoute(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("inject-unknown"))

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "existing-route",
		Policy: routing.RoutePolicy{
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	<-receiver.Ready()

	err := rt.Inject(context.Background(), "nonexistent", messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x"}))
	if err == nil {
		t.Fatal("expected error for unknown route")
	}
}

// Verifies Inject returns an error when the runtime has not been started.
func TestRuntime_Inject_NotRunning(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("inject-stopped"))

	err := rt.Inject(context.Background(), "any-route", messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x"}))
	if err == nil {
		t.Fatal("expected error when runtime is not running")
	}
}

// Verifies Inject assigns an ID on the cloned envelope when the input has no ID without mutating the original.
func TestRuntime_Inject_AssignsIDWhenEmpty(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("inject-id"),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "id-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte("no-id")})
	originalID := env.ID()
	if err := rt.Inject(context.Background(), "id-route", env); err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	waitFor(t, time.Second, "inject with auto ID sent", func() bool {
		return sender.SentCount() == 1
	})

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if sender.GetSent()[0].ID() == "" {
		t.Fatal("injected envelope should have an ID")
	}
	if env.ID() != originalID {
		t.Fatal("original envelope should not be mutated (clone)")
	}
}

// Verifies Inject does not mutate the caller's envelope headers after processing.
func TestRuntime_Inject_DoesNotMutateOriginal(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("inject-clone"),
		goruntime.WithDLQStore(NewFakeDLQStore()),
	)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID: "clone-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
	_ = rt.AddRoute(cfg, receiver, sender, nil, nil)
	_ = rt.Start(context.Background())
	defer func() { _ = rt.Stop(context.Background()) }()

	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "orig-id",
		Headers: map[string]any{"keep": "this"},
	})
	_ = rt.Inject(context.Background(), "clone-route", env)

	waitFor(t, time.Second, "clone-route inject completed", func() bool {
		return sender.SentCount() == 1
	})

	if len(env.Headers()) != 1 {
		t.Fatalf("original headers should not be modified, got %d entries", len(env.Headers()))
	}
	if env.Headers()["keep"] != "this" {
		t.Fatal("original header value should be preserved")
	}
}

// Verifies shared-outbox delivery: message is acked after persist, outbox records appear, drain sends to destination.
//
// Scenario: start runtime with sess lease and drain; emit one message; assert ack, outbox, then at least one send after drain.
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
	sess := NewFakeSession()

	sessCfg := session.DefaultConfig("sess-e2e", true)
	sessCfg.LeaseTTL = 500 * time.Millisecond
	sessCfg.RenewInterval = 100 * time.Millisecond
	sessCfg.DrainStrategy = persistence.NewFixedPoll(50 * time.Millisecond)

	cfg := goruntime.RouteConfig{
		ID: "outbox-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "bind-e2e", Address: "topic/e2e"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "bind-e2e", SessionID: "sess-e2e"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)
	ctx := context.Background()
	_ = rt.Start(ctx)

	<-receiver.Ready()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "outbox-msg", Payload: []byte("outbox-data")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, time.Second, "outbox persist and source ack", func() bool {
		return del.IsAcked() && outbox.RecordCount() > 0
	})

	if !del.IsAcked() {
		t.Fatal("delivery should be acked after outbox persist")
	}
	if outbox.RecordCount() == 0 {
		t.Fatal("expected outbox records")
	}

	waitFor(t, 5*time.Second, "outbox drain sent to destination", func() bool {
		return sender.SentCount() >= 1
	})

	if sender.SentCount() < 1 {
		t.Fatalf("expected at least 1 sent from outbox drain, got %d", sender.SentCount())
	}

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = rt.Stop(stopCtx)
}

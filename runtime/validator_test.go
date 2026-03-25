package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

func validDirectHoldEntry() (runtime.RouteConfig, ports.Receiver, ports.Sender, ports.Session, *runtime.SessionConfig) {
	cfg := runtime.RouteConfig{
		ID: "sqs-to-mqtt-dh",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			DispatchMode: domain.DispatchSingle,
		},
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}
	sessCfg := runtime.DefaultSessionConfig("mqtt-gateway", false)
	return cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &sessCfg
}

func TestValidator_DirectHold_Valid(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rt.Start(ctx)
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("valid direct_hold route should pass validation, got: %v", err)
	}
}

func TestValidator_DirectHold_RejectsFanOut(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.DispatchMode = domain.DispatchFanOut

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for fan-out in direct_hold")
	}
	if !strings.Contains(err.Error(), "direct_hold invalid: resolver fan-out is enabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidator_DirectHold_RejectsExclusiveSession(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, _ := validDirectHoldEntry()
	exclusiveCfg := runtime.DefaultSessionConfig("mqtt-exclusive", true)

	if err := rt.AddRoute(cfg, rx, tx, sess, &exclusiveCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for exclusive session in direct_hold")
	}
	if !strings.Contains(err.Error(), "direct_hold invalid: target session requires lease handoff") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidator_DirectHold_RejectsMissingVisibilityExtension(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.SourceCapabilities = []ports.Capability{ports.CapSourceRedelivery}

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for missing visibility extension")
	}
	if !strings.Contains(err.Error(), "direct_hold invalid: source does not support visibility extension") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidator_DirectHold_RejectsMultipleBindings(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Bindings = []domain.DestinationBinding{
		{ID: "bind-a", SessionID: "sess-a"},
		{ID: "bind-b", SessionID: "sess-b"},
	}

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for multiple bindings in direct_hold")
	}
	if !strings.Contains(err.Error(), "direct_hold invalid: multiple bindings") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidator_DirectHold_CollectsMultipleErrors(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, _ := validDirectHoldEntry()
	cfg.Policy.DispatchMode = domain.DispatchFanOut
	cfg.SourceCapabilities = nil
	exclusiveCfg := runtime.DefaultSessionConfig("mqtt-exclusive", true)

	if err := rt.AddRoute(cfg, rx, tx, sess, &exclusiveCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation errors")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "fan-out") {
		t.Error("missing fan-out error")
	}
	if !strings.Contains(errMsg, "lease handoff") {
		t.Error("missing lease handoff error")
	}
	if !strings.Contains(errMsg, "visibility extension") {
		t.Error("missing visibility extension error")
	}
}

func TestValidator_DirectHold_NoSession(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, _, _ := validDirectHoldEntry()

	if err := rt.AddRoute(cfg, rx, tx, nil, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rt.Start(ctx)
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("direct_hold without session should pass validation, got: %v", err)
	}
}

func TestValidator_SharedOutbox_Valid(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(outbox),
		runtime.WithLeaseStore(lease),
	)

	cfg := runtime.RouteConfig{
		ID: "sqs-to-mqtt-so",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []domain.DispatchPlan{{BindingID: "b1", Address: "topic/a"}},
		},
	}
	sessCfg := runtime.DefaultSessionConfig("mqtt-sess", true)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &sessCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rt.Start(ctx)
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("valid shared_outbox route should pass validation, got: %v", err)
	}
}

func TestValidator_SharedOutbox_RejectsMissingOutboxStore(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))

	cfg := runtime.RouteConfig{
		ID: "sqs-to-mqtt-no-outbox",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
	}

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), nil, nil); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for missing OutboxStore")
	}
	if !strings.Contains(err.Error(), "shared_outbox invalid: no OutboxStore configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidator_DirectHold_DefaultDeliveryMode(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))

	cfg := runtime.RouteConfig{
		ID:                 "default-mode",
		Policy:             domain.RoutePolicy{},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), nil, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rt.Start(ctx)
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("default delivery mode (direct_hold) should pass validation, got: %v", err)
	}
}

func TestValidator_MultipleRouteErrors(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))

	cfg1 := runtime.RouteConfig{
		ID: "bad-route-1",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			DispatchMode: domain.DispatchFanOut,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	cfg2 := runtime.RouteConfig{
		ID: "bad-route-2",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
	}

	_ = rt.AddRoute(cfg1, NewFakeReceiver(), NewFakeSender(), nil, nil)
	_ = rt.AddRoute(cfg2, NewFakeReceiver(), NewFakeSender(), nil, nil)

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation errors from both routes")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "bad-route-1") {
		t.Error("missing error from bad-route-1")
	}
	if !strings.Contains(errMsg, "bad-route-2") {
		t.Error("missing error from bad-route-2")
	}
}

func TestValidator_SharedOutbox_NonExclusiveNoLeaseStore(t *testing.T) {
	outbox := NewFakeOutboxStore()
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(outbox),
	)

	cfg := runtime.RouteConfig{
		ID: "non-exclusive-so",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
	}
	nonExclusiveCfg := runtime.DefaultSessionConfig("mqtt-persistent", false)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &nonExclusiveCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rt.Start(ctx)
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("shared_outbox with non-exclusive session and no lease store should pass, got: %v", err)
	}
}

func TestValidator_SharedOutbox_RejectsMissingLeaseStoreForExclusive(t *testing.T) {
	outbox := NewFakeOutboxStore()
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(outbox),
	)

	cfg := runtime.RouteConfig{
		ID: "sqs-to-mqtt-no-lease",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
	}
	exclusiveCfg := runtime.DefaultSessionConfig("mqtt-exclusive", true)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &exclusiveCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for missing LeaseStore with exclusive session")
	}
	if !strings.Contains(err.Error(), "shared_outbox invalid: no LeaseStore configured for exclusive session") {
		t.Fatalf("unexpected error: %v", err)
	}
}

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

// TestValidator_DirectHold_Valid verifies a well-formed direct_hold route passes start validation.
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

// TestValidator_DirectHold_RejectsFanOut verifies fan-out dispatch is rejected for direct_hold.
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

// TestValidator_DirectHold_RejectsExclusiveSession verifies exclusive target sessions are rejected for direct_hold.
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

// TestValidator_DirectHold_RejectsMissingVisibilityExtension verifies direct_hold requires visibility extension on the source.
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

// TestValidator_DirectHold_RejectsMultipleBindings verifies more than one binding is rejected for direct_hold.
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

// TestValidator_DirectHold_CollectsMultipleErrors verifies several direct_hold violations appear together in the start error.
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

// TestValidator_DirectHold_NoSession verifies direct_hold is valid when no session is configured.
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

// TestValidator_SharedOutbox_Valid verifies a well-formed shared_outbox route with outbox and lease stores passes validation.
func TestValidator_SharedOutbox_Valid(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(outbox),
		runtime.WithLeaseStore(lease),
		runtime.WithDLQStore(NewFakeDLQStore()),
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

// TestValidator_SharedOutbox_RejectsMissingOutboxStore verifies shared_outbox without OutboxStore fails validation.
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

// TestValidator_DirectHold_DefaultDeliveryMode verifies default policy implies direct_hold and passes when capabilities match.
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

// TestValidator_MultipleRouteErrors verifies start aggregates validation failures from multiple routes.
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

// TestValidator_SharedOutbox_NonExclusiveNoLeaseStore verifies non-exclusive session allows missing LeaseStore.
func TestValidator_SharedOutbox_NonExclusiveNoLeaseStore(t *testing.T) {
	outbox := NewFakeOutboxStore()
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(outbox),
		runtime.WithDLQStore(NewFakeDLQStore()),
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

// TestValidationError_Errors_ReturnsAllErrors validates that Errors() returns a copy
// of all collected error messages and that mutating the copy does not affect the original.
func TestValidationError_Errors_ReturnsAllErrors(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-verr"))

	cfg1 := runtime.RouteConfig{
		ID: "bad-1",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			DispatchMode: domain.DispatchFanOut,
		},
	}
	_ = rt.AddRoute(cfg1, NewFakeReceiver(), NewFakeSender(), nil, nil)

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error")
	}

	ve, ok := err.(*runtime.ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}

	errs := ve.Errors()
	if len(errs) == 0 {
		t.Fatal("expected at least one error message")
	}

	original := errs[0]
	errs[0] = "mutated"

	errs2 := ve.Errors()
	if errs2[0] != original {
		t.Fatalf("Errors() did not return a copy; mutation was visible: %q vs %q", errs2[0], original)
	}
}

// TestValidator_SharedOutbox_RejectsMissingLeaseStoreForExclusive verifies exclusive shared_outbox requires LeaseStore.
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

// TestValidator_DirectHold_MultiBindingsWithResolver verifies that multiple
// bindings are allowed in direct_hold when a resolver is configured.
func TestValidator_DirectHold_MultiBindingsWithResolver(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Bindings = []domain.DestinationBinding{
		{ID: "bind-a", SessionID: "sess-a"},
		{ID: "bind-b", SessionID: "sess-b"},
	}
	// Setting a resolver relaxes the multi-binding validation.
	cfg.Resolver = runtime.NewStaticResolver(domain.DispatchPlan{
		BindingID: "bind-a", Address: "topic-a",
	})

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := rt.Start(ctx)
	if err != nil {
		t.Fatalf("expected no validation error with resolver + multi-bindings, got: %v", err)
	}
	_ = rt.Stop(context.Background())
}

// TestValidator_DirectHold_FanOutStillRejectedWithResolver verifies that
// fan-out dispatch is still rejected in direct_hold even with a resolver.
func TestValidator_DirectHold_FanOutStillRejectedWithResolver(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.DispatchMode = domain.DispatchFanOut
	cfg.Resolver = runtime.NewStaticResolver(domain.DispatchPlan{BindingID: "b"})

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for fan-out in direct_hold even with resolver")
	}
	if !strings.Contains(err.Error(), "fan-out") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_DirectHold_HTTPSourceAccepted verifies that HTTP sources
// (CapHTTPEndpoint) are accepted in direct_hold without CapVisibilityExtension.
func TestValidator_DirectHold_HTTPSourceAccepted(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.SourceCapabilities = []ports.Capability{ports.CapHTTPEndpoint}

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := rt.Start(ctx)
	if err != nil {
		t.Fatalf("expected no validation error for HTTP source, got: %v", err)
	}
	_ = rt.Stop(context.Background())
}

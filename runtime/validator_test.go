package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

func validDirectHoldEntry() (runtime.RouteConfig, ports.Receiver, ports.Sender, ports.Session, *session.Config) {
	cfg := runtime.RouteConfig{
		ID: "sqs-to-mqtt-dh",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchSingle,
		},
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}
	sessCfg := session.DefaultConfig("mqtt-gateway", false)
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
	cfg.Policy.DispatchMode = routing.DispatchFanOut

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
	exclusiveCfg := session.DefaultConfig("mqtt-exclusive", true)

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
	cfg.Bindings = []routing.DestinationBinding{
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
	cfg.Policy.DispatchMode = routing.DispatchFanOut
	cfg.SourceCapabilities = nil
	exclusiveCfg := session.DefaultConfig("mqtt-exclusive", true)

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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{{BindingID: "b1", Address: "topic/a"}},
		},
	}
	sessCfg := session.DefaultConfig("mqtt-sess", true)

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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
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
		Policy:             routing.RoutePolicy{},
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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchFanOut,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	cfg2 := runtime.RouteConfig{
		ID: "bad-route-2",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
	}
	nonExclusiveCfg := session.DefaultConfig("mqtt-persistent", false)

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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchFanOut,
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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
	}
	exclusiveCfg := session.DefaultConfig("mqtt-exclusive", true)

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
	cfg.Bindings = []routing.DestinationBinding{
		{ID: "bind-a", SessionID: "sess-a"},
		{ID: "bind-b", SessionID: "sess-b"},
	}
	// Setting a resolver relaxes the multi-binding validation.
	cfg.Resolver = runtime.NewStaticResolver(routing.DispatchPlan{
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
	cfg.Policy.DispatchMode = routing.DispatchFanOut
	cfg.Resolver = runtime.NewStaticResolver(routing.DispatchPlan{BindingID: "b"})

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

// TestValidator_SharedOutbox_FanOutExceedsTransactionLimit verifies that
// shared_outbox rejects fan-out when binding count exceeds the outbox
// transaction limit.
func TestValidator_SharedOutbox_FanOutExceedsTransactionLimit(t *testing.T) {
	outbox := NewFakeOutboxStore()
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(outbox),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	bindings := make([]routing.DestinationBinding, 101)
	for i := range bindings {
		bindings[i] = routing.DestinationBinding{
			ID:       "b" + strings.Repeat("x", i),
			SenderID: "sender-1",
		}
	}

	cfg := runtime.RouteConfig{
		ID: "fanout-overflow",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			DispatchMode: routing.DispatchFanOut,
		},
		Bindings: bindings,
	}
	sessCfg := session.DefaultConfig("sess", false)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for fan-out exceeding transaction limit")
	}
	if !strings.Contains(err.Error(), "transaction limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_SharedOutbox_FanOutAtLimit verifies shared_outbox accepts
// fan-out when binding count equals the transaction limit.
func TestValidator_SharedOutbox_FanOutAtLimit(t *testing.T) {
	outbox := NewFakeOutboxStore()
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(outbox),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	bindings := make([]routing.DestinationBinding, 100)
	for i := range bindings {
		bindings[i] = routing.DestinationBinding{
			ID:       "b" + strings.Repeat("x", i),
			SenderID: "sender-1",
		}
	}

	cfg := runtime.RouteConfig{
		ID: "fanout-at-limit",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			DispatchMode: routing.DispatchFanOut,
		},
		Bindings: bindings,
	}
	sessCfg := session.DefaultConfig("sess", false)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &sessCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rt.Start(ctx)
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected pass for fan-out at limit, got: %v", err)
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

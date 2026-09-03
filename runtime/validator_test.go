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
			DeliveryMode:       routing.DeliveryDirectHold,
			DispatchMode:       routing.DispatchSingle,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
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

// TestValidator_DirectHold_RejectsASourceThatCannotRedeliver pins the mode's real
// precondition. direct_hold settles the source only after the destination has
// accepted, so what it needs is a source that hands the message back when the
// process dies mid-flight. A window it can extend is one way to get that and not
// the requirement — a source that offers the window and never redelivers leaves
// the crash gap the mode exists to close.
func TestValidator_DirectHold_RejectsASourceThatCannotRedeliver(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.SourceCapabilities = []ports.Capability{ports.CapVisibilityExtension}
	cfg.SourceRedeliveryRefusal = "subscription \"sensors/#\" is QoS 0"

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for a source that cannot redeliver")
	}
	if !strings.Contains(err.Error(), "direct_hold invalid: the source does not redeliver an unsettled message") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "is QoS 0") {
		t.Fatalf("the refusal must carry the transport's own reason so an operator knows which "+
			"precondition failed: %v", err)
	}
}

// TestValidator_DirectHold_AcceptsARedeliveringSourceWithNoWindow is the other
// direction, and the one that was refused before: a source with no visibility
// window at all is admissible when it redelivers what it was never told to
// settle. MQTT on a durable session is exactly that, and forcing it through an
// outbox added a store, a lease and a failure domain for a crash window that is
// identical either way.
func TestValidator_DirectHold_AcceptsARedeliveringSourceWithNoWindow(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.SourceCapabilities = []ports.Capability{ports.CapSourceRedelivery}

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("expected no validation error for a redelivering source, got: %v", err)
	}
	_ = rt.Stop(context.Background())
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
	if !strings.Contains(errMsg, "does not redeliver an unsettled message") {
		t.Error("missing source-redelivery error")
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

// TestValidator_SharedOutbox_RejectsZeroPlanStaticResolver verifies a
// shared_outbox route whose StaticResolver is fixed at ZERO plans is rejected at
// registration. Such a resolver would persist zero outbox records for
// every message and then ACK the source with no delivery — silent loss. Because
// the cardinality is statically knowable, the misconfiguration must fail fast at
// Start, mirroring the direct_hold PlanCount()>1 rejection.
//
// Mutation check: delete the StaticResolver PlanCount()==0 branch in
// validateSharedOutbox and this fails — Start returns nil (the empty resolver is
// accepted and only fails per-message at runtime).
func TestValidator_SharedOutbox_RejectsZeroPlanStaticResolver(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(NewFakeOutboxStore()),
		runtime.WithLeaseStore(NewFakeLeaseStore()),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "sqs-to-mqtt-zero-plans",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: runtime.NewStaticResolver(), // ZERO plans — statically knowable
	}
	sessCfg := session.DefaultConfig("mqtt-sess", true)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for a static resolver yielding 0 dispatch plans")
	}
	if !strings.Contains(err.Error(), "yields 0 dispatch plans") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_DirectHold_RejectsZeroPlanStaticResolver verifies the zero-plan
// static-resolver rejection fires for direct_hold too (mode-agnostic). A
// direct_hold route configured with NewStaticResolver() can never produce a
// delivery — it would retry-poison EVERY message — so this knowable-dead config
// must fail fast at Start, mirroring how a statically-zero resolver is a dead
// config in any delivery mode.
//
// Mutation check: scope the zero-plan rejection back to shared_outbox only and
// this fails — the direct_hold route starts (Start returns nil for validation).
func TestValidator_DirectHold_RejectsZeroPlanStaticResolver(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.ID = "sqs-to-mqtt-dh-zero-plans"
	cfg.Resolver = runtime.NewStaticResolver() // ZERO plans — statically knowable

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for a direct_hold static resolver yielding 0 dispatch plans")
	}
	if !strings.Contains(err.Error(), "yields 0 dispatch plans") {
		t.Fatalf("unexpected error: %v", err)
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
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID:                 "default-mode",
		Policy:             routing.RoutePolicy{},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
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
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
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

// TestValidator_SharedOutbox_NonExclusiveSession_Rejected verifies that
// finding 11 is enforced: a shared_outbox route bound to a non-exclusive
// session is rejected at validation. A non-exclusive session never acquires
// a lease, so its outbox drainer's TokenFn reports "not held" every cycle and
// the partition never drains — persisted records would silently strand. The
// combo must therefore fail fast instead of ACKing the source into a black hole.
func TestValidator_SharedOutbox_NonExclusiveSession_Rejected(t *testing.T) {
	outbox := NewFakeOutboxStore()
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(outbox),
		runtime.WithLeaseStore(NewFakeLeaseStore()),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "non-exclusive-so",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{{ID: "b1", Address: "topic/x"}},
	}
	nonExclusiveCfg := session.DefaultConfig("mqtt-persistent", false)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &nonExclusiveCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for shared_outbox on a non-exclusive session")
	}
	if !strings.Contains(err.Error(), "non-exclusive") {
		t.Fatalf("expected non-exclusive rejection, got: %v", err)
	}
	_ = rt.Stop(context.Background())
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
		runtime.WithLeaseStore(NewFakeLeaseStore()),
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
	// shared_outbox requires an exclusive session with a lease store
	// (finding 11); the subject here is the fan-out cardinality limit.
	sessCfg := session.DefaultConfig("sess", true)

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
// (CapHTTPEndpoint) are accepted in direct_hold without CapSourceRedelivery: the
// caller is still holding the request, so nothing has been settled and the retry
// is theirs to make.
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

// TestValidator_TerminalFailureSink_PermanentDLQNoStore_Rejected verifies that a
// route whose effective on_permanent_failure routes to the DLQ is rejected when
// no DLQ store is configured (terminal failures must not be silently dropped).
func TestValidator_TerminalFailureSink_PermanentDLQNoStore_Rejected(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.OnPermanentFailure = routing.FailureDLQ
	cfg.Policy.OnExpired = routing.ExpiredDrop

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for on_permanent_failure=dlq without DLQ store")
	}
	if !strings.Contains(err.Error(), "on_permanent_failure=dlq but no DLQ store configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_TerminalFailureSink_ExpiredDLQNoStore_Rejected verifies that a
// route whose effective on_expired routes to the DLQ is rejected when no DLQ
// store is configured.
func TestValidator_TerminalFailureSink_ExpiredDLQNoStore_Rejected(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.OnPermanentFailure = routing.FailureDrop
	cfg.Policy.OnExpired = routing.ExpiredDLQ

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for on_expired=dlq without DLQ store")
	}
	if !strings.Contains(err.Error(), "on_expired=dlq but no DLQ store configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_TerminalFailureSink_DLQWithStore_Accepted verifies that routing
// terminal outcomes to the DLQ passes validation once a DLQ store is configured.
func TestValidator_TerminalFailureSink_DLQWithStore_Accepted(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.OnPermanentFailure = routing.FailureDLQ
	cfg.Policy.OnExpired = routing.ExpiredDLQ

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rt.Start(ctx)
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("terminal DLQ routing with a DLQ store should pass validation, got: %v", err)
	}
}

// TestValidator_SharedOutbox_RejectsZeroBindingsNoResolver asserts that a
// shared_outbox route with neither bindings nor a resolver is rejected at Start.
// With no bindings the dispatcher falls back to the plan {BindingID: routeID};
// routeID matches no binding, so the record's session resolves to "" and it
// persists under BINDING#<routeID> — a partition no drainer in any instance ever
// polls. The source is ACKed after persist and the record is silently lost, so
// validation must fail closed.
func TestValidator_SharedOutbox_RejectsZeroBindingsNoResolver(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(NewFakeOutboxStore()),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "zero-binding-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		// No Bindings and no Resolver.
	}

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), nil, nil); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for shared_outbox route with no bindings and no resolver")
	}
	if !strings.Contains(err.Error(), "has no bindings and no resolver") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_SharedOutbox_CrossInstanceIngressOnly_Validates locks in the
// no-false-positive contract: an ingress-only instance whose binding targets a
// non-empty session that has NO drainer in THIS runtime must still validate.
// This is the core cross-instance handoff — a different instance owns the
// session lease and drains the shared outbox. Local drainer absence is not proof
// of orphaning, so the per-instance validator must not reject it.
func TestValidator_SharedOutbox_CrossInstanceIngressOnly_Validates(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("ingress-only-bridge"),
		runtime.WithOutboxStore(NewFakeOutboxStore()),
		runtime.WithLeaseStore(NewFakeLeaseStore()),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "ingress-only-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			// Non-empty session owned and drained by a DIFFERENT instance.
			{ID: "b1", SessionID: "remote-owned-sess", Address: "topic/a"},
		},
	}

	// nil session + nil sessCfg and no RegisterSessionSender → no local drainer
	// for "remote-owned-sess". This must still validate.
	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), nil, nil); err != nil {
		t.Fatal(err)
	}

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("ingress-only cross-instance route must validate (remote instance drains): %v", err)
	}
	_ = rt.Stop(context.Background())
}

// TestValidator_SharedOutbox_RejectsExplicitTargetAccept verifies that an
// explicit ack_after=target_accept on a shared_outbox route is rejected because
// the outbox persist — not the downstream accept — is the durability boundary.
func TestValidator_SharedOutbox_RejectsExplicitTargetAccept(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(NewFakeOutboxStore()),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "sqs-to-mqtt-so-ackafter",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			AckAfter:     routing.AckAfterTargetAccept,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{{BindingID: "b1", Address: "topic/a"}},
		},
	}
	sessCfg := session.DefaultConfig("mqtt-sess", false)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for explicit ack_after=target_accept on shared_outbox")
	}
	if !strings.Contains(err.Error(), "ack_after=target_accept is not honored") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_SharedOutbox_RejectsOrphanBinding asserts that a shared_outbox
// route with no route session and a binding that omits its session_id is
// rejected at Start. With no session to inherit (the fixup needs a route
// session) and an empty binding session, the outbox record would persist under
// a BINDING#<id> partition that no drainer ever polls — the source is ACKed
// after persist and the record is silently lost. Validation must fail closed.
func TestValidator_SharedOutbox_RejectsOrphanBinding(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(NewFakeOutboxStore()),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "orphan-binding-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b-orphan", Address: "topic/a"}, // no SessionID, nothing to inherit
		},
	}

	// nil session + nil sessCfg → the empty binding session stays empty.
	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), nil, nil); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for shared_outbox binding with no session to drain")
	}
	if !strings.Contains(err.Error(), "has no session_id and the route has no session to inherit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_SharedOutbox_BindingInheritsRouteSession asserts the inverse of
// the orphan case: an empty binding session is fine when the route has a
// session for it to inherit, so validation passes.
func TestValidator_SharedOutbox_BindingInheritsRouteSession(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithOutboxStore(NewFakeOutboxStore()),
		runtime.WithLeaseStore(NewFakeLeaseStore()),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "inherit-binding-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b-inherit", Address: "topic/a"}, // no SessionID → inherits route session
		},
	}
	// shared_outbox requires an exclusive session (finding 11); the subject
	// here is that an empty binding session inherits the route session.
	sessCfg := session.DefaultConfig("route-sess", true)

	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &sessCfg); err != nil {
		t.Fatal(err)
	}

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("expected route to validate (binding inherits route session): %v", err)
	}
	_ = rt.Stop(context.Background())
}

// TestValidator_RejectsBackoffMultiplierBelowOne pins the runtime start
// boundary to the same rule the config boundary enforces. A multiplier in (0,1)
// survives WithDefaults (which fills only ZERO fields) and turns "exponential
// backoff" into accelerating retry: every attempt fires sooner than the last
// until the delay underflows, so a failing target is hammered hardest exactly
// when it is least able to recover.
func TestValidator_RejectsBackoffMultiplierBelowOne(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.Backoff.Multiplier = 0.5

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("a decaying retry multiplier must fail start validation")
	}
	if !strings.Contains(err.Error(), "Multiplier") {
		t.Fatalf("error must name the offending field: %v", err)
	}
}

// TestValidator_AcceptsBackoffMultiplierOne is the negative control: a fixed
// retry interval (multiplier exactly 1) is a legitimate policy, so the rule is
// `>= 1`, not `> 1`.
func TestValidator_AcceptsBackoffMultiplierOne(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.Backoff.Multiplier = 1.0

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Start(ctx); err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("multiplier 1.0 is a legal fixed retry interval: %v", err)
	}
}

package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// TestInjectToBinding_SharedOutbox_ConfinesRedriveToOneBinding is the
// regression test for the binding-scoped DLQ redrive defect.
//
// The admin redrive used to carry the target binding in the reserved header
// x-bridge.route-override, then call Inject. But doHandleDelivery strips every
// reserved header at ingress (the security property that stops external
// messages from steering routing) BEFORE the shared_outbox consumption site
// reads the override — so a redrive of ONE failed fan-out leg fell through to
// full resolution and re-persisted outbox records for ALL bindings, delivering
// duplicates to the N-1 healthy legs.
//
// InjectToBinding carries the binding out-of-band on the synthetic delivery
// (surviving the strip), so a redrive confined to binding-a persists exactly
// one outbox record, for binding-a. With the defect present this test observes
// two records (binding-a AND binding-b) and fails.
func TestInjectToBinding_SharedOutbox_ConfinesRedriveToOneBinding(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-redrive-scope", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-sess-redrive")

	// Fan-out shared_outbox route: the resolver yields BOTH legs, so a normal
	// delivery persists a record per binding. A binding-scoped redrive must not.
	cfg := goruntime.RouteConfig{
		ID: "fanout-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "binding-a", Address: "devices/a/state"},
				{BindingID: "binding-b", Address: "devices/b/state"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-a", Address: "devices/a/state"},
			{ID: "binding-b", Address: "devices/b/state"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", sess.IsStarted)

	// Redrive confined to binding-a only (exactly what the admin DLQ redrive
	// does for a single failed fan-out leg). InjectToBinding is synchronous
	// through the outbox persist, so records are settled when it returns.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "redrive-msg-1",
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})
	if err := rt.InjectToBinding(ctx, "fanout-route", "binding-a", env); err != nil {
		t.Fatalf("InjectToBinding: %v", err)
	}

	records := outbox.Records()
	if len(records) != 1 {
		got := make([]string, len(records))
		for i, rec := range records {
			got[i] = rec.BindingID()
		}
		t.Fatalf("expected exactly 1 outbox record confined to binding-a, got %d for bindings %v", len(records), got)
	}
	if got := records[0].BindingID(); got != "binding-a" {
		t.Fatalf("expected outbox record for binding-a, got %q", got)
	}
}

// TestInject_SharedOutbox_FanOutPersistsAllBindings is the control for the
// regression: a plain Inject (no binding override) on the same fan-out
// route persists a record for EVERY binding. It documents the pre-fix
// behaviour that a binding-scoped redrive must avoid, and guards against a
// regression that would over-confine ordinary deliveries.
func TestInject_SharedOutbox_FanOutPersistsAllBindings(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-fanout-control", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-sess-fanout")

	cfg := goruntime.RouteConfig{
		ID: "fanout-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "binding-a", Address: "devices/a/state"},
				{BindingID: "binding-b", Address: "devices/b/state"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-a", Address: "devices/a/state"},
			{ID: "binding-b", Address: "devices/b/state"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", sess.IsStarted)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "fanout-msg-1",
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})
	if err := rt.Inject(ctx, "fanout-route", env); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	bindings := map[string]bool{}
	for _, rec := range outbox.Records() {
		bindings[rec.BindingID()] = true
	}
	if !bindings["binding-a"] || !bindings["binding-b"] {
		t.Fatalf("expected fan-out records for both bindings, got %v", bindings)
	}
}

package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Regression tests for the DLQ-redrive silent-loss defect (audit,
// CRITICAL): redriving a shared-outbox message under its ORIGINAL envelope ID
// collides with the outbox's retained dedup row (UNIQUE envelope_id,
// binding_id — completed/poisoned rows are kept as evidence), so Persist
// returns ErrDuplicateRecord, the dispatch path ACKs it as already-persisted,
// the redrive reports success and the DLQ entry is deleted — while the
// message is never sent again.
//
// Runtime.InjectRedrive fixes this at the injection choke point: the message
// is re-issued under a FRESH envelope ID with the original ID stamped as
// provenance (messaging.HeaderCausationID) after the ingress strip.

func redriveFreshIDSetup(t *testing.T) (*goruntime.Runtime, *FakeOutboxStore, *FakeSession) {
	t.Helper()
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-redrive-fresh-id", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-sess-redrive-fresh")

	cfg := goruntime.RouteConfig{
		ID: "outbox-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-a", Address: "devices/a/state"},
		},
	}
	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	waitFor(t, 2*time.Second, "sess started", sess.IsStarted)
	return rt, outbox, sess
}

// TestInjectRedrive_SharedOutbox_FreshIDBypassesRetainedDedupRow replays the
// loss sequence: a record for the original envelope ID is already retained
// in the outbox (here: persisted by the original delivery), then the operator
// redrives the same message. InjectRedrive must persist a NEW record under a
// fresh envelope ID carrying x-bridge.causation-id = original ID. The pre-fix
// path (InjectToBinding with the original ID) is swallowed by dedup — see the
// control test below.
func TestInjectRedrive_SharedOutbox_FreshIDBypassesRetainedDedupRow(t *testing.T) {
	rt, outbox, _ := redriveFreshIDSetup(t)
	ctx := context.Background()

	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "orig-env-1",
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})

	// Original delivery persists the record that will be retained as the
	// dedup evidence row (completed rows are kept by real stores; the fake
	// dedups on (envelope_id, binding_id) across all statuses, same key).
	if err := rt.Inject(ctx, "outbox-route", orig); err != nil {
		t.Fatalf("Inject (original delivery): %v", err)
	}
	if got := outbox.RecordCount(); got != 1 {
		t.Fatalf("original delivery: record count got %d, want 1", got)
	}

	// Operator redrive of the SAME message (same envelope ID).
	if err := rt.InjectRedrive(ctx, "outbox-route", "binding-a", orig); err != nil {
		t.Fatalf("InjectRedrive: %v", err)
	}

	records := outbox.Records()
	if len(records) != 2 {
		t.Fatalf("redrive must persist a NEW outbox record; record count got %d, want 2 "+
			"(the redrive was silently swallowed by the retained dedup row)", len(records))
	}

	var redriven bool
	for _, rec := range records {
		if rec.EnvelopeID() == "orig-env-1" {
			continue // the original delivery's record
		}
		redriven = true
		if rec.EnvelopeID() == "" {
			t.Fatalf("redriven record has empty envelope ID")
		}
		env := rec.Snapshot()
		got, ok := env.Header(messaging.HeaderCausationID)
		if !ok || got != "orig-env-1" {
			t.Fatalf("redriven record must carry provenance %s=orig-env-1, got %v (present=%v)",
				messaging.HeaderCausationID, got, ok)
		}
		if env.Subject() != "device.state.update" {
			t.Fatalf("redriven record subject got %q, want original subject", env.Subject())
		}
		if string(env.Payload()) != "hello" {
			t.Fatalf("redriven record payload got %q, want original payload", env.Payload())
		}
	}
	if !redriven {
		t.Fatalf("no redriven record with a fresh envelope ID found among %d records", len(records))
	}
}

// TestInjectToBinding_SharedOutbox_OriginalIDIsSwallowedByDedup is the
// CONTROL documenting the hazard the redrive-safe path exists to avoid:
// injecting under the original envelope ID against a retained row returns
// SUCCESS (the duplicate is ACKed as already-persisted) while persisting
// nothing — the caller cannot distinguish this from a real replay. Guards the
// dedup contract the fix depends on; if this ever starts persisting a second
// record, InjectRedrive's fresh-ID rationale must be revisited.
func TestInjectToBinding_SharedOutbox_OriginalIDIsSwallowedByDedup(t *testing.T) {
	rt, outbox, _ := redriveFreshIDSetup(t)
	ctx := context.Background()

	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "orig-env-2",
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})
	if err := rt.Inject(ctx, "outbox-route", orig); err != nil {
		t.Fatalf("Inject (original delivery): %v", err)
	}

	// Pre-fix redrive path: same envelope ID, swallowed silently.
	if err := rt.InjectToBinding(ctx, "outbox-route", "binding-a", orig); err != nil {
		t.Fatalf("InjectToBinding returned error, want silent duplicate-ack: %v", err)
	}
	if got := outbox.RecordCount(); got != 1 {
		t.Fatalf("dedup contract changed: record count got %d, want 1", got)
	}
}

// TestInjectRedrive_EmptyID_FallsBackToPlainInject: an envelope without an ID
// (only constructible as a zero value — both envelope constructors require an
// ID) has nothing to collide with; InjectRedrive must behave like a plain
// binding-scoped inject (fresh ID assigned, no provenance header). Defensive
// branch: DLQ entries always retain envelopes with IDs.
func TestInjectRedrive_EmptyID_FallsBackToPlainInject(t *testing.T) {
	rt, outbox, _ := redriveFreshIDSetup(t)
	ctx := context.Background()

	if err := rt.InjectRedrive(ctx, "outbox-route", "binding-a", &messaging.Envelope{}); err != nil {
		t.Fatalf("InjectRedrive: %v", err)
	}
	records := outbox.Records()
	if len(records) != 1 {
		t.Fatalf("record count got %d, want 1", len(records))
	}
	if _, ok := records[0].Snapshot().Header(messaging.HeaderCausationID); ok {
		t.Fatalf("no-ID redrive must not stamp provenance")
	}
}

// TestInjectRedrive_StripsStaleDedupHeader pins [FINDING 2]: a fresh envelope ID
// alone does NOT make a redrive safe when the DLQ'd envelope carries
// x-bridge.dedup-id — an idempotent/FIFO sender prefers that header over the ID
// (e.g. SQS FIFO maps it to MessageDeduplicationId), so a copied-through dedup
// key would make the transport swallow the "fresh" redrive (ACK without
// delivering) → Send returns nil → the admin handler deletes the DLQ entry after
// a no-op. InjectRedrive must therefore DROP the stale dedup key so the sender
// re-derives dedup from the fresh envelope ID, while keeping non-suppressing
// keys (correlation/ordering) and the causation provenance.
//
// The route uses TrustBridgeHeaders so the propagated header class (dedup-id,
// ordering-key, …) survives the ingress hop — exactly the deployment where a
// stale dedup key would otherwise reach the sender (the default posture strips
// ALL x-bridge.* at ingress, masking both the bug and the fix).
//
// Mutation reasoning — remove the `fresh.DeleteHeader(HeaderDeduplicationID)`
// line in InjectRedrive and this test fails: the redriven record carries
// x-bridge.dedup-id = "orig-dedup" (the original key rides through).
func TestInjectRedrive_StripsStaleDedupHeader(t *testing.T) {
	rt, outbox := redriveTrustedHeaderSetup(t)
	ctx := context.Background()

	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:              "orig-env-dedup",
		Subject:         "device.state.update",
		Payload:         []byte("hello"),
		DeduplicationID: "orig-dedup", // stamped as x-bridge.dedup-id
		OrderingKey:     "order-1",    // must be preserved (does not suppress)
	})

	if err := rt.InjectRedrive(ctx, "outbox-route", "binding-a", orig); err != nil {
		t.Fatalf("InjectRedrive: %v", err)
	}

	records := outbox.Records()
	if len(records) != 1 {
		t.Fatalf("record count got %d, want 1", len(records))
	}
	env := records[0].Snapshot()

	// Fresh ID, not the original.
	if freshID := env.ID(); freshID == "orig-env-dedup" || freshID == "" {
		t.Fatalf("redriven record must carry a fresh non-empty envelope ID, got %q", freshID)
	}

	// The stale transport dedup key must be GONE so the sender re-derives dedup
	// from the fresh ID (the core FINDING 2 assertion).
	if v, ok := env.Header(messaging.HeaderDeduplicationID); ok {
		t.Fatalf("redrive must strip the stale x-bridge.dedup-id, but it rode through as %v", v)
	}

	// Non-suppressing keys and provenance survive (route trusts bridge headers).
	if v, ok := env.Header(messaging.HeaderOrderingKey); !ok || v != "order-1" {
		t.Fatalf("ordering-key must be preserved on redrive, got %v (present=%v)", v, ok)
	}
	if v, ok := env.Header(messaging.HeaderCausationID); !ok || v != "orig-env-dedup" {
		t.Fatalf("redrive must stamp causation provenance = original ID, got %v (present=%v)", v, ok)
	}
}

// redriveTrustedHeaderSetup builds a shared_outbox route with TrustBridgeHeaders
// enabled, so the BRIDGE-TO-BRIDGE PROPAGATED header class (dedup-id,
// ordering-key, …) survives the ingress hop onto the persisted outbox record.
func redriveTrustedHeaderSetup(t *testing.T) (*goruntime.Runtime, *FakeOutboxStore) {
	t.Helper()
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-redrive-trusted", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-sess-redrive-trusted")

	cfg := goruntime.RouteConfig{
		ID: "outbox-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliverySharedOutbox,
			TrustBridgeHeaders: true,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-a", Address: "devices/a/state"},
		},
	}
	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	waitFor(t, 2*time.Second, "sess started", sess.IsStarted)
	return rt, outbox
}

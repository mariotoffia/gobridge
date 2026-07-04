package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Regression tests for the DLQ-redrive silent-loss defect (audit D1,
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
// D1 loss sequence: a record for the original envelope ID is already retained
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
		t.Fatalf("D1: redrive must persist a NEW outbox record; record count got %d, want 2 "+
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
			t.Fatalf("D1: redriven record must carry provenance %s=orig-env-1, got %v (present=%v)",
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
// CONTROL documenting the D1 hazard the redrive-safe path exists to avoid:
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

package routing_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// TestDLQEntry_NewDLQEntry_ClonesEnvelope proves NewDLQEntry deep-clones
// the spec envelope so a later mutation of the caller's envelope cannot
// reach the entry (finding #9: snapshot boundary).
func TestDLQEntry_NewDLQEntry_ClonesEnvelope(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-dlq",
		Subject: "subj",
		Headers: map[string]any{"trace": "t0"},
	})
	entry := routing.NewDLQEntry(routing.DLQEntrySpec{ID: "d1", Envelope: *env})

	env.SetHeader("trace", "mutated")
	env.SetHeader("added", "late")

	got := entry.Snapshot()
	if v, _ := got.Header("trace"); v != "t0" {
		t.Fatalf("NewDLQEntry did not isolate envelope: trace=%v, want t0", v)
	}
	if _, ok := got.Header("added"); ok {
		t.Fatal("late caller header leaked into entry")
	}
}

// TestDLQEntry_Snapshot_ReturnsIndependentClone proves Snapshot hands out a
// clone whose mutation cannot corrupt the entry or a later snapshot.
func TestDLQEntry_Snapshot_ReturnsIndependentClone(t *testing.T) {
	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:       "d2",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-d2", Headers: map[string]any{"trace": "t0"}}),
	})

	first := entry.Snapshot()
	first.SetHeader("trace", "mutated")

	second := entry.Snapshot()
	if v, _ := second.Header("trace"); v != "t0" {
		t.Fatalf("Snapshot result aliased entry state: trace=%v, want t0", v)
	}
}

// TestDLQEntry_RehydrateDLQEntry_DoesNotClone pins the documented contract
// that the storage rehydration constructor takes ownership of the supplied
// envelope WITHOUT a redundant deep clone, so adapters materializing from a
// freshly decoded row pay only one copy. Mutating the source envelope is
// therefore observable through the entry.
func TestDLQEntry_RehydrateDLQEntry_DoesNotClone(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-rh",
		Headers: map[string]any{"trace": "t0"},
	})
	entry := routing.RehydrateDLQEntry(routing.DLQEntrySpec{ID: "d3", Envelope: *env})

	env.SetHeader("trace", "mutated")
	if v, _ := entry.Snapshot().Header("trace"); v != "mutated" {
		t.Fatalf("RehydrateDLQEntry unexpectedly cloned the envelope: trace=%v, want mutated", v)
	}
}

// TestDLQEntry_Accessors verifies every value-receiver accessor returns the
// value that was set via DLQEntrySpec, covering the full field→accessor mapping
// introduced when the 13 exported fields were privatised.
func TestDLQEntry_Accessors(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	spec := routing.DLQEntrySpec{
		ID:            "acc-1",
		RouteID:       "route-acc",
		BindingID:     "bind-acc",
		Address:       "addr-acc",
		SessionID:     "sess-acc",
		SourceID:      "src-acc",
		CorrelationID: "corr-acc",
		Reason:        "reason-acc",
		Category:      "cat-acc",
		ErrorCode:     "ERR_ACC",
		LastError:     "last-error-acc",
		FailedAt:      now,
		Attempts:      7,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: "env-acc", Subject: "test/acc",
		}),
	}

	entry := routing.RehydrateDLQEntry(spec)

	if entry.ID() != "acc-1" {
		t.Errorf("ID: got %q, want %q", entry.ID(), "acc-1")
	}
	if entry.RouteID() != "route-acc" {
		t.Errorf("RouteID: got %q, want %q", entry.RouteID(), "route-acc")
	}
	if entry.BindingID() != "bind-acc" {
		t.Errorf("BindingID: got %q, want %q", entry.BindingID(), "bind-acc")
	}
	if entry.Address() != "addr-acc" {
		t.Errorf("Address: got %q, want %q", entry.Address(), "addr-acc")
	}
	if entry.SessionID() != "sess-acc" {
		t.Errorf("SessionID: got %q, want %q", entry.SessionID(), "sess-acc")
	}
	if entry.SourceID() != "src-acc" {
		t.Errorf("SourceID: got %q, want %q", entry.SourceID(), "src-acc")
	}
	if entry.CorrelationID() != "corr-acc" {
		t.Errorf("CorrelationID: got %q, want %q", entry.CorrelationID(), "corr-acc")
	}
	if entry.Reason() != "reason-acc" {
		t.Errorf("Reason: got %q, want %q", entry.Reason(), "reason-acc")
	}
	if entry.Category() != "cat-acc" {
		t.Errorf("Category: got %q, want %q", entry.Category(), "cat-acc")
	}
	if entry.ErrorCode() != "ERR_ACC" {
		t.Errorf("ErrorCode: got %q, want %q", entry.ErrorCode(), "ERR_ACC")
	}
	if entry.LastError() != "last-error-acc" {
		t.Errorf("LastError: got %q, want %q", entry.LastError(), "last-error-acc")
	}
	if !entry.FailedAt().Equal(now) {
		t.Errorf("FailedAt: got %v, want %v", entry.FailedAt(), now)
	}
	if entry.Attempts() != 7 {
		t.Errorf("Attempts: got %d, want 7", entry.Attempts())
	}
}

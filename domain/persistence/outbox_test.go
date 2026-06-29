package persistence_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// TestOutboxPartitionKey_WithSession validates the "SESSION#<id>" format when sessionID is non-empty.
func TestOutboxPartitionKey_WithSession(t *testing.T) {
	key := persistence.OutboxPartitionKey("sess-1", "bind-1")
	if key != "SESSION#sess-1" {
		t.Fatalf("got %q, want %q", key, "SESSION#sess-1")
	}
}

// TestOutboxPartitionKey_WithBinding validates the "BINDING#<id>" format when sessionID is empty.
func TestOutboxPartitionKey_WithBinding(t *testing.T) {
	key := persistence.OutboxPartitionKey("", "bind-1")
	if key != "BINDING#bind-1" {
		t.Fatalf("got %q, want %q", key, "BINDING#bind-1")
	}
}

// TestOutboxPartitionKey_Deterministic validates that the same inputs always produce the same key.
func TestOutboxPartitionKey_Deterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		a := persistence.OutboxPartitionKey("s1", "b1")
		b := persistence.OutboxPartitionKey("s1", "b1")
		if a != b {
			t.Fatalf("iteration %d: keys diverged: %q vs %q", i, a, b)
		}
	}
}

func newDispatchRecord(t *testing.T, dh map[string]any) *persistence.OutboxRecord {
	t.Helper()
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:              "rec-dh",
		RouteID:         "route-dh",
		EnvelopeID:      "env-dh",
		BindingID:       "bind-dh",
		SessionID:       "sess-dh",
		Address:         "addr",
		Envelope:        *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-dh"}),
		DispatchHeaders: dh,
	})
	if err != nil {
		t.Fatalf("NewOutboxRecord: %v", err)
	}
	return rec
}

// TestOutboxRecord_DispatchHeaders_InputMapMutationDoesNotLeak proves that
// mutating the spec's DispatchHeaders map (including nested maps) after
// construction cannot reach into the aggregate (finding #8: deep copy at
// construction).
func TestOutboxRecord_DispatchHeaders_InputMapMutationDoesNotLeak(t *testing.T) {
	input := map[string]any{"k": "v0", "nested": map[string]any{"inner": "i0"}}
	rec := newDispatchRecord(t, input)

	input["k"] = "mutated"
	input["added"] = "late"
	input["nested"].(map[string]any)["inner"] = "mutated"

	got := rec.DispatchHeaders()
	if got["k"] != "v0" {
		t.Fatalf("top-level header leaked: got %v, want v0", got["k"])
	}
	if _, ok := got["added"]; ok {
		t.Fatal("late-added input key leaked into record")
	}
	if got["nested"].(map[string]any)["inner"] != "i0" {
		t.Fatalf("nested header leaked: got %v, want i0", got["nested"].(map[string]any)["inner"])
	}
}

// TestOutboxRecord_DispatchHeaders_AccessorReturnsDeepCopy proves the
// accessor hands out an isolated deep copy on every call, so mutating one
// result cannot corrupt the record or a later accessor result.
func TestOutboxRecord_DispatchHeaders_AccessorReturnsDeepCopy(t *testing.T) {
	rec := newDispatchRecord(t, map[string]any{"k": "v0", "nested": map[string]any{"inner": "i0"}})

	first := rec.DispatchHeaders()
	first["k"] = "mutated"
	first["added"] = "late"
	first["nested"].(map[string]any)["inner"] = "mutated"

	second := rec.DispatchHeaders()
	if second["k"] != "v0" {
		t.Fatalf("accessor result aliased: got %v, want v0", second["k"])
	}
	if _, ok := second["added"]; ok {
		t.Fatal("mutation of accessor result leaked back into record")
	}
	if second["nested"].(map[string]any)["inner"] != "i0" {
		t.Fatalf("nested accessor result aliased: got %v, want i0", second["nested"].(map[string]any)["inner"])
	}
}

// TestOutboxRecord_PersistenceSnapshot_RehydrateIsolatesDispatchHeaders
// proves the persistence round-trip is fully isolated: the snapshot DTO's
// map is a copy, and a record rehydrated from a PersistenceSnapshot does
// not alias the source record's dispatch headers (finding #8 aliasing-bug
// fix).
func TestOutboxRecord_PersistenceSnapshot_RehydrateIsolatesDispatchHeaders(t *testing.T) {
	rec := newDispatchRecord(t, map[string]any{"k": "v0"})

	snap := rec.PersistenceSnapshot()
	snap.DispatchHeaders["k"] = "mutated-snap"
	snap.DispatchHeaders["added"] = "late"
	if rec.DispatchHeaders()["k"] != "v0" {
		t.Fatalf("snapshot DTO map aliased the source record: got %v", rec.DispatchHeaders()["k"])
	}

	clone := persistence.RehydrateFromSnapshot(rec.PersistenceSnapshot())
	cloneDH := clone.DispatchHeaders()
	cloneDH["k"] = "mutated-clone"
	if rec.DispatchHeaders()["k"] != "v0" {
		t.Fatalf("mutating rehydrated record dispatch headers corrupted source: got %v", rec.DispatchHeaders()["k"])
	}
	if clone.DispatchHeaders()["k"] != "v0" {
		t.Fatalf("rehydrated record accessor not isolated: got %v", clone.DispatchHeaders()["k"])
	}
}

// TestOutboxRecord_OrderingKey verifies finding #28: OrderingKey reads
// the messaging.HeaderOrderingKey header from the embedded envelope and
// reports presence only for a non-empty string key.
func TestOutboxRecord_OrderingKey(t *testing.T) {
	mk := func(headers map[string]any) *persistence.OutboxRecord {
		env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{ID: "env-ok", Headers: headers})
		rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
			ID:         "rec-ok",
			RouteID:    "route-ok",
			EnvelopeID: "env-ok",
			BindingID:  "bind-ok",
			SessionID:  "sess-ok",
			Envelope:   *env,
		})
		if err != nil {
			t.Fatalf("NewOutboxRecord: %v", err)
		}
		return rec
	}

	t.Run("present", func(t *testing.T) {
		rec := mk(map[string]any{messaging.HeaderOrderingKey: "device-42"})
		key, ok := rec.OrderingKey()
		if !ok || key != "device-42" {
			t.Fatalf("OrderingKey: got (%q,%v), want (device-42,true)", key, ok)
		}
	})

	t.Run("absent", func(t *testing.T) {
		if key, ok := mk(nil).OrderingKey(); ok || key != "" {
			t.Fatalf("OrderingKey absent: got (%q,%v), want (\"\",false)", key, ok)
		}
	})

	t.Run("empty", func(t *testing.T) {
		rec := mk(map[string]any{messaging.HeaderOrderingKey: ""})
		if key, ok := rec.OrderingKey(); ok || key != "" {
			t.Fatalf("OrderingKey empty value: got (%q,%v), want (\"\",false)", key, ok)
		}
	})

	t.Run("non-string", func(t *testing.T) {
		rec := mk(map[string]any{messaging.HeaderOrderingKey: 123})
		if key, ok := rec.OrderingKey(); ok || key != "" {
			t.Fatalf("OrderingKey non-string: got (%q,%v), want (\"\",false)", key, ok)
		}
	})
}

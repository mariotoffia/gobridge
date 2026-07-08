package sqlitedlq

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/routing"
)

func mustStore(t *testing.T, path string, opts ...Option) *Store {
	t.Helper()
	s, err := NewStore(path, opts...)
	if err != nil {
		t.Fatalf("NewStore(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func dlqEntry(id string, failedAt time.Time) routing.DLQEntry {
	return routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:       id,
		RouteID:  "route-1",
		Category: "timeout",
		FailedAt: failedAt,
	})
}

// seed writes directly through the session, bypassing Store.Write's throttled
// sweep so the throttle bookkeeping (lastSweep) is untouched by test setup.
func seed(t *testing.T, s *Store, id string, failedAt time.Time) {
	t.Helper()
	if err := s.sess.write(context.Background(), dlqEntry(id, failedAt)); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func pragmaSynchronous(t *testing.T, s *Store) int {
	t.Helper()
	var mode int
	if err := s.sess.db.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil {
		t.Fatalf("read PRAGMA synchronous: %v", err)
	}
	return mode
}

func listIDs(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	entries, err := s.List(context.Background(), routing.DLQFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make(map[string]bool, len(entries))
	for _, e := range entries {
		ids[e.ID()] = true
	}
	return ids
}

// TestSynchronousPinnedFull asserts the durability pin: PRAGMA synchronous
// resolves to 2 (FULL) so a committed DLQ entry survives OS/power loss rather
// than resting on WAL-mode's NORMAL default or an unpinned driver default.
func TestSynchronousPinnedFull(t *testing.T) {
	s := mustStore(t, ":memory:")
	if got := pragmaSynchronous(t, s); got != 2 {
		t.Fatalf("PRAGMA synchronous = %d, want 2 (FULL)", got)
	}
}

// TestSynchronousPinOverridesDSNRequest is the counterfactual with teeth:
// modernc defaults to FULL, so a bare assertion would pass even without the
// pin. Opening over a DSN that explicitly requests synchronous=NORMAL(1) and
// still observing FULL(2) proves the pragma pin (not a default) is in force.
func TestSynchronousPinOverridesDSNRequest(t *testing.T) {
	s := mustStore(t, ":memory:?_pragma=synchronous(1)")
	if got := pragmaSynchronous(t, s); got != 2 {
		t.Fatalf("PRAGMA synchronous after a DSN NORMAL request: got %d, want 2 (FULL) — the pin is missing", got)
	}
}

// TestRetentionSweepPurgesExpiredOnWrite proves the opt-in retention sweep:
// with WithRetention configured, a Write triggers a purge of entries older than
// the window while in-window entries survive. Deterministic via an injected
// clock (no time.Sleep).
func TestRetentionSweepPurgesExpiredOnWrite(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	nowT := base
	s := mustStore(t, ":memory:", WithRetention(time.Hour), WithClock(clocktest.NewAt(nowT)))

	// Seed (no sweep) an already-expired entry and an in-window entry.
	seed(t, s, "expired", base.Add(-2*time.Hour))
	seed(t, s, "fresh", base.Add(-10*time.Minute))

	// A single Store.Write fires the (un-throttled first) sweep at cutoff
	// base-1h: it purges 'expired' and keeps 'fresh' and the trigger.
	if err := s.Write(context.Background(), dlqEntry("trigger", base)); err != nil {
		t.Fatalf("write trigger: %v", err)
	}

	ids := listIDs(t, s)
	if ids["expired"] {
		t.Fatalf("expired entry (failed_at=base-2h) should have been swept, present in %v", ids)
	}
	if !ids["fresh"] || !ids["trigger"] {
		t.Fatalf("in-window entries must survive the sweep, got %v", ids)
	}
}

// TestRetentionDisabledByDefault proves the sweep is opt-in: without
// WithRetention, an expired entry is retained indefinitely (historical
// behaviour) — no silent data loss for callers that never asked for retention.
func TestRetentionDisabledByDefault(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	s := mustStore(t, ":memory:") // no WithRetention; sweep never runs, clock unused

	seed(t, s, "ancient", base.Add(-1000*time.Hour))
	if err := s.Write(context.Background(), dlqEntry("trigger", base)); err != nil {
		t.Fatalf("write trigger: %v", err)
	}

	ids := listIDs(t, s)
	if !ids["ancient"] {
		t.Fatalf("with retention disabled, ancient entry must be retained, got %v", ids)
	}
}

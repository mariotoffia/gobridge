package sqlitedlq

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
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

// ═══════════════════════════════════════════════════════════════════
// c10-dlq-fatal — fatal storage-fault classification + store-health metric
// ═══════════════════════════════════════════════════════════════════

// recordingMeter is a counterMeter that captures every emitted counter so a
// test can assert the store-health signal fires exactly once with the right
// dimension. Safe for the -race detector.
type recordingMeter struct {
	mu   sync.Mutex
	hits []meterHit
}

type meterHit struct {
	name  string
	value int64
	tags  []shared.Tag
}

func (m *recordingMeter) Counter(name string, value int64, tags ...shared.Tag) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hits = append(m.hits, meterHit{name: name, value: value, tags: append([]shared.Tag(nil), tags...)})
}

func (m *recordingMeter) count(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.hits {
		if h.name == name {
			n++
		}
	}
	return n
}

func (m *recordingMeter) hasTag(name, key, val string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.hits {
		if h.name != name {
			continue
		}
		for _, tg := range h.tags {
			if tg.Key == key && tg.Value == val {
				return true
			}
		}
	}
	return false
}

func assertPermanent(t *testing.T, err error) {
	t.Helper()
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("error is not a *shared.BridgeError: %v", err)
	}
	if be.Class != shared.ErrorPermanent {
		t.Fatalf("error class: got %q, want %q (err=%v)", be.Class, shared.ErrorPermanent, err)
	}
}

// TestFatalStorageError_ClassifiedPermanent is the direct mutation guard for
// c10-dlq-fatal: a fatal storage fault (read-only / disk-full / corrupt /
// not-a-database) must map to a PERMANENT BridgeError, NOT the transient
// ErrConnectionLost/ErrUnavailable fall-through. Remove the isFatalStorageErr
// branch from mapError and every case here fails (the string falls through to
// ErrUnavailable, which is not ErrorPermanent).
func TestFatalStorageError_ClassifiedPermanent(t *testing.T) {
	for _, msg := range []string{
		"attempt to write a readonly database",
		"database or disk is full",
		"database disk image is malformed",
		"file is not a database",
	} {
		t.Run(msg, func(t *testing.T) {
			out := mapError(errors.New(msg))
			assertPermanent(t, out)
			if errors.Is(out, shared.ErrUnavailable) || errors.Is(out, shared.ErrConnectionLost) {
				t.Fatalf("fatal storage fault must NOT be transient, got %v", out)
			}
			if errors.Is(out, shared.ErrThrottled) {
				t.Fatalf("fatal storage fault must NOT be throttled, got %v", out)
			}
		})
	}
}

// TestFatalStorageFaultClassifiedPermanentAndCounted proves the fault is both
// classified PERMANENT and counted via MetricStoreUnhealthy with entity=dlq,
// exercised through BOTH the open path (a non-database file -> SQLITE_NOTADB)
// and a runtime write path (query_only -> SQLITE_READONLY). The DLQ is the
// last-resort sink, so its fatal faults must not blend into transient noise.
func TestFatalStorageFaultClassifiedPermanentAndCounted(t *testing.T) {
	// Open path: a file that is not a database surfaces NOTADB permanently.
	garbage := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(garbage, []byte("definitely not a sqlite database, just prose"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := NewStore(garbage); err == nil {
		_ = s.Close()
		t.Fatal("expected NewStore over a non-database file to fail")
	} else {
		assertPermanent(t, err)
	}

	// Runtime path: query_only makes the write INSERT fail with READONLY.
	meter := &recordingMeter{}
	s := mustStore(t, ":memory:", WithMetrics(meter))

	if _, err := s.sess.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatalf("arm query_only: %v", err)
	}

	werr := s.Write(context.Background(), dlqEntry("ro-1", time.Unix(1_700_000_000, 0)))
	if werr == nil {
		t.Fatal("expected Write under query_only to fail")
	}
	assertPermanent(t, werr)

	if got := meter.count(MetricStoreUnhealthy); got != 1 {
		t.Fatalf("MetricStoreUnhealthy count: got %d, want 1", got)
	}
	if !meter.hasTag(MetricStoreUnhealthy, shared.TagKeyEntity, "dlq") {
		t.Fatalf("MetricStoreUnhealthy must carry %s=dlq", shared.TagKeyEntity)
	}
}

// TestTransientFaultNotCountedAsFatal proves a transient busy/locked fault is
// NOT misclassified as a fatal store-health event: it stays transient and emits
// no MetricStoreUnhealthy counter, so the alertable signal keeps its meaning.
func TestTransientFaultNotCountedAsFatal(t *testing.T) {
	if isFatalStorageErr(errors.New("database is locked (5) (SQLITE_BUSY)")) {
		t.Fatal("SQLITE_BUSY must not be classified as a fatal storage fault")
	}
	meter := &recordingMeter{}
	s := mustStore(t, ":memory:", WithMetrics(meter))

	// observe over a transient error must not emit the halt counter.
	_ = s.observe(context.Background(), shared.ErrThrottled.Wrap(errors.New("database is locked")))
	if got := meter.count(MetricStoreUnhealthy); got != 0 {
		t.Fatalf("transient error emitted %d MetricStoreUnhealthy counters, want 0", got)
	}
}

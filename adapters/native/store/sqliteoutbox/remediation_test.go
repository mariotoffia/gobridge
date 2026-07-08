package sqliteoutbox

// Internal regression tests for the production-readiness remediation:
//   - H2  partition-scoped duplicate identity (schema rebuild migration).
//   - MED fatal storage-fault classification + store-health metric.
//   - MED PRAGMA synchronous=FULL pin (counterfactual over a DSN override).
//   - LOW partial compaction indexes are actually used by the sweep DELETEs.
//   - MIN duplicate IDs within one Complete/Release batch no longer trip a
//         spurious stale-fence error.
//
// They live in the production package because they inspect raw schema, row
// counts, and query plans through the session handle — the public port
// surface deliberately cannot observe any of that. The shared storetest kit
// owns the cross-backend behavioural assertion (partition-scoped Persist).

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// --- shared test helpers ---------------------------------------------------

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

func pragmaInt(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	var v int64
	if err := s.sess.db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
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

func indexExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.sess.db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name,
	).Scan(&n); err != nil {
		t.Fatalf("probe index %s: %v", name, err)
	}
	return n == 1
}

func queryPlan(t *testing.T, s *Store, query string) string {
	t.Helper()
	rows, err := s.sess.db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		b.WriteString(detail)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return b.String()
}

// mustRecordIdent builds a record with explicit envelope+binding+session IDs so
// a test can pin the (partition key, EnvelopeID, BindingID) identity directly.
func mustRecordIdent(t *testing.T, id, envID, bindID, sess string, createdAt time.Time) *persistence.OutboxRecord {
	t.Helper()
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    "route-1",
		EnvelopeID: envID,
		BindingID:  bindID,
		SessionID:  sess,
		Address:    "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      envID,
			Subject: "test-subject",
			Payload: []byte(`{}`),
		}),
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("new record %s: %v", id, err)
	}
	return rec
}

// --- MED: PRAGMA synchronous=FULL pin --------------------------------------

// A freshly opened store runs at synchronous=FULL (2). modernc defaults to
// FULL, so this alone is not proof the pin works — see the DSN-override
// counterfactual below.
func TestSynchronousPinnedFull(t *testing.T) {
	s, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := pragmaInt(t, s, "synchronous"); got != 2 {
		t.Fatalf("PRAGMA synchronous: got %d, want 2 (FULL)", got)
	}
}

// Counterfactual with teeth: the store is opened over a DSN that explicitly
// requests synchronous=NORMAL(1). openSession must re-assert FULL(2). Remove
// the `PRAGMA synchronous=FULL` line and this reads 1 and fails — proving the
// pin, not the driver default, is what guarantees durability.
func TestSynchronousPinOverridesDSNRequest(t *testing.T) {
	s, err := NewStore(":memory:?_pragma=synchronous(1)")
	if err != nil {
		t.Fatalf("NewStore over NORMAL DSN: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := pragmaInt(t, s, "synchronous"); got != 2 {
		t.Fatalf("PRAGMA synchronous after a DSN NORMAL request: got %d, want 2 (FULL) — the pin is missing", got)
	}
}

// --- MED: fatal storage-fault classification + store-health metric ---------

// A fatal storage fault is classified PERMANENT (so the drain loop stops
// retrying) and counted via MetricStoreUnhealthy (a distinct halt signal),
// exercised through BOTH the open path (a non-database file -> SQLITE_NOTADB)
// and a runtime write path (query_only -> SQLITE_READONLY).
func TestFatalStorageFaultClassifiedPermanentAndCounted(t *testing.T) {
	// Open path: a file that is not a database surfaces NOTADB permanently.
	garbage := t.TempDir() + "/garbage.db"
	if err := os.WriteFile(garbage, []byte("definitely not a sqlite database, just prose"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(garbage); err == nil {
		t.Fatal("expected NewStore over a non-database file to fail")
	} else {
		assertPermanent(t, err)
	}

	// Runtime path: query_only makes the persist INSERT fail with READONLY.
	meter := &recordingMeter{}
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s, err := NewStore(":memory:", WithMetrics(meter), WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.sess.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatalf("arm query_only: %v", err)
	}

	rec := mustRecordIdent(t, "ro-1", "env-ro", "bind-ro", "sess-ro", clk.Now())
	perr := s.Persist(context.Background(), []*persistence.OutboxRecord{rec})
	if perr == nil {
		t.Fatal("expected Persist under query_only to fail")
	}
	assertPermanent(t, perr)

	if got := meter.count(MetricStoreUnhealthy); got != 1 {
		t.Fatalf("MetricStoreUnhealthy count: got %d, want 1", got)
	}
	if !meter.hasTag(MetricStoreUnhealthy, shared.TagKeyEntity, "outbox") {
		t.Fatalf("MetricStoreUnhealthy must carry %s=outbox", shared.TagKeyEntity)
	}
}

// A transient fault must NOT be misclassified as a fatal store-health event: a
// busy/locked error stays transient and emits no MetricStoreUnhealthy counter.
func TestTransientFaultNotCountedAsFatal(t *testing.T) {
	if isFatalStorageErr(errors.New("database is locked (5) (SQLITE_BUSY)")) {
		t.Fatal("SQLITE_BUSY must not be classified as a fatal storage fault")
	}
	meter := &recordingMeter{}
	s, err := NewStore(":memory:", WithMetrics(meter))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	// observe over a transient error must not emit the halt counter.
	_ = s.observe(context.Background(), shared.ErrThrottled.Wrap(errors.New("database is locked")))
	if got := meter.count(MetricStoreUnhealthy); got != 0 {
		t.Fatalf("transient error emitted %d MetricStoreUnhealthy counters, want 0", got)
	}
}

// --- H2: identity migration (schema rebuild) -------------------------------

// A database carrying the legacy GLOBAL UNIQUE(envelope_id, binding_id) is
// rebuilt in place: the inline constraint is dropped, the partition-scoped
// idx_outbox_identity replaces it, existing rows survive, and the Persist
// identity is now partition-scoped (same envelope+binding under a new
// partition persists; under the same partition it is still a duplicate).
func TestIdentityMigrationDropsGlobalUniqueAndScopesByPartition(t *testing.T) {
	dbPath := t.TempDir() + "/legacy-identity.db"

	legacy, err := openRawForTest(dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	const legacySchema = `
CREATE TABLE outbox (
    id             TEXT PRIMARY KEY,
    partition_key  TEXT NOT NULL,
    route_id       TEXT NOT NULL,
    envelope_id    TEXT NOT NULL,
    binding_id     TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    address        TEXT NOT NULL DEFAULT '',
    envelope_json  TEXT NOT NULL,
    headers_json   TEXT,
    status         TEXT NOT NULL DEFAULT 'pending',
    claimed_by     TEXT NOT NULL DEFAULT '',
    claim_version  INTEGER NOT NULL DEFAULT 0,
    replay_count   INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL DEFAULT 0,
    completed_at   INTEGER NOT NULL DEFAULT 0,
    UNIQUE(envelope_id, binding_id)
);
CREATE TABLE outbox_partition_fence (
    partition_key TEXT PRIMARY KEY,
    max_version   INTEGER NOT NULL DEFAULT 0
);`
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	t0 := time.Unix(1_700_000_000, 0)
	if _, err := legacy.Exec(
		`INSERT INTO outbox (id, partition_key, route_id, envelope_id, binding_id, session_id, envelope_json, created_at)
		 VALUES ('leg-1', 'SESSION#sess-A', 'route-1', 'env-x', 'bind-x', 'sess-A', '{"id":"env-x","subject":"s"}', ?)`,
		t0.UnixMilli(),
	); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	clk := clocktest.NewAt(t0.Add(time.Hour))
	s, err := NewStore(dbPath, WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore over legacy identity db: %v", err)
	}
	defer func() { _ = s.Close() }()

	// (1) The rebuilt table no longer carries the global inline UNIQUE.
	var ddl string
	if err := s.sess.db.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'outbox'",
	).Scan(&ddl); err != nil {
		t.Fatalf("read outbox ddl: %v", err)
	}
	if strings.Contains(strings.ReplaceAll(ddl, " ", ""), "UNIQUE(envelope_id,binding_id)") {
		t.Fatalf("legacy global UNIQUE was not dropped by the migration:\n%s", ddl)
	}
	// (2) The partition-scoped identity index exists.
	if !indexExists(t, s, "idx_outbox_identity") {
		t.Fatal("idx_outbox_identity not created by the migration")
	}
	// (3) The legacy row survived the rebuild.
	if got := countRows(t, s, "outbox"); got != 1 {
		t.Fatalf("legacy row lost in rebuild, rows=%d", got)
	}

	ctx := context.Background()
	// (4) Same (envelope, binding) under a DIFFERENT partition must persist.
	other := mustRecordIdent(t, "new-B", "env-x", "bind-x", "sess-B", clk.Now())
	if err := s.Persist(ctx, []*persistence.OutboxRecord{other}); err != nil {
		t.Fatalf("cross-partition re-persist of same identity must succeed, got %v", err)
	}
	// (5) Same (envelope, binding) under the SAME partition is still a duplicate.
	dup := mustRecordIdent(t, "dup-A", "env-x", "bind-x", "sess-A", clk.Now())
	if err := s.Persist(ctx, []*persistence.OutboxRecord{dup}); !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("same-partition duplicate must return ErrDuplicateRecord, got %v", err)
	}
}

// --- LOW: partial compaction indexes are used -------------------------------

// The retention-compaction DELETEs must hit the partial indexes, not full-scan
// the table. Removing the partial indexes turns these into "SCAN outbox".
func TestRetentionDeletesUsePartialIndexes(t *testing.T) {
	s, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, tc := range []struct{ query, index string }{
		{"DELETE FROM outbox WHERE status = 'completed' AND completed_at > 0 AND completed_at < 1", "idx_outbox_completed"},
		{"DELETE FROM outbox WHERE status = 'expired' AND expires_at > 0 AND expires_at < 1", "idx_outbox_expired"},
	} {
		plan := queryPlan(t, s, tc.query)
		if !strings.Contains(plan, tc.index) {
			t.Fatalf("compaction DELETE did not use %s (full scan?):\n%s", tc.index, plan)
		}
	}
}

// --- MIN: duplicate IDs within one Complete/Release batch -------------------

// A Complete batch that lists the same record ID twice must succeed: the
// physical row is completed once and the duplicate is collapsed, so the
// fence-count guard does not fire a spurious ErrStaleFencingToken. Without the
// in-batch dedup, RowsAffected (1) < len(recordIDs) (2) and this errors.
func TestCompleteDedupsDuplicateBatchIDs(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s, err := NewStore(":memory:", WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	if err := s.Persist(ctx, []*persistence.OutboxRecord{mustRecordIdent(t, "cd-1", "env-cd", "bind-cd", "sess-cd", clk.Now())}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := s.Claim(ctx, "SESSION#sess-cd", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := s.Complete(ctx, []string{"cd-1", "cd-1"}, token); err != nil {
		t.Fatalf("Complete with duplicate IDs must succeed, got %v", err)
	}
	if got := countRows(t, s, "outbox"); got != 1 {
		t.Fatalf("record row count: got %d, want 1", got)
	}
	pending, err := s.QueryPending(ctx, "SESSION#sess-cd", 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("record must be completed (0 pending), got %d", len(pending))
	}
}

// A Release batch that lists the same record ID twice must succeed and return
// the record to pending, again without a spurious stale-fence error.
func TestReleaseDedupsDuplicateBatchIDs(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s, err := NewStore(":memory:", WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	if err := s.Persist(ctx, []*persistence.OutboxRecord{mustRecordIdent(t, "rd-1", "env-rd", "bind-rd", "sess-rd", clk.Now())}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := s.Claim(ctx, "SESSION#sess-rd", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := s.Release(ctx, []string{"rd-1", "rd-1"}, token); err != nil {
		t.Fatalf("Release with duplicate IDs must succeed, got %v", err)
	}
	pending, err := s.QueryPending(ctx, "SESSION#sess-rd", 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("released record must be pending again, got %d pending", len(pending))
	}
}

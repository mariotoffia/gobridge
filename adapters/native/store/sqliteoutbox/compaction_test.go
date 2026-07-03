package sqliteoutbox

// Internal tests for retention compaction (WithRetention) and the additive
// schema migrations (seq column, fence updated_at). They live in the
// production package because they must inspect raw row counts through the
// session handle — the public port surface deliberately cannot observe
// terminal rows.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// openRawForTest opens a bare database/sql handle so a test can lay down a
// legacy schema before NewStore migrates it. The modernc driver is already
// registered by this package's ACL.
func openRawForTest(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path)
}

func mustRecordAt(t *testing.T, id, sess string, createdAt, expiresAt time.Time) *persistence.OutboxRecord {
	t.Helper()
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    "route-1",
		EnvelopeID: "env-" + id,
		BindingID:  "bind-" + id,
		SessionID:  sess,
		Address:    "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-" + id,
			Subject: "test-subject",
			Payload: []byte(`{}`),
		}),
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("new record %s: %v", id, err)
	}
	return rec
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.sess.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Completed rows older than the retention window are physically deleted by
// the compaction pass piggybacked on Complete; fresh terminal rows survive.
func TestRetentionCompactsCompletedRows(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clk := clocktest.NewAt(t0)
	const retention = 30 * time.Minute

	s, err := NewStore(":memory:", WithClock(clk), WithRetention(retention))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	// Complete record A at t0.
	if err := s.Persist(ctx, []*persistence.OutboxRecord{mustRecordAt(t, "ra", "sess-cmp", t0, time.Time{})}); err != nil {
		t.Fatalf("persist A: %v", err)
	}
	if _, err := s.Claim(ctx, "SESSION#sess-cmp", token, 10); err != nil {
		t.Fatalf("claim A: %v", err)
	}
	if err := s.Complete(ctx, []string{"ra"}, token); err != nil {
		t.Fatalf("complete A: %v", err)
	}
	if got := countRows(t, s, "outbox"); got != 1 {
		t.Fatalf("A must survive compaction inside the retention window, rows=%d", got)
	}

	// One retention window later, complete record B: the piggybacked pass
	// must delete A (terminal since t0) and keep B.
	clk.Advance(retention + time.Minute)
	if err := s.Persist(ctx, []*persistence.OutboxRecord{mustRecordAt(t, "rb", "sess-cmp", clk.Now(), time.Time{})}); err != nil {
		t.Fatalf("persist B: %v", err)
	}
	if _, err := s.Claim(ctx, "SESSION#sess-cmp", token, 10); err != nil {
		t.Fatalf("claim B: %v", err)
	}
	if err := s.Complete(ctx, []string{"rb"}, token); err != nil {
		t.Fatalf("complete B: %v", err)
	}

	if got := countRows(t, s, "outbox"); got != 1 {
		t.Fatalf("expected exactly 1 row after compaction (B), got %d", got)
	}
	var id string
	if err := s.sess.db.QueryRow("SELECT id FROM outbox").Scan(&id); err != nil {
		t.Fatalf("select survivor: %v", err)
	}
	if id != "rb" {
		t.Fatalf("survivor: got %q, want %q", id, "rb")
	}
}

// Expired rows past retention are deleted on the pass piggybacked on Expire,
// and fence rows of partitions untouched for max(retention, 30d) are dropped.
func TestRetentionCompactsExpiredRowsAndStaleFences(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clk := clocktest.NewAt(t0)
	const retention = 30 * time.Minute

	s, err := NewStore(":memory:", WithClock(clk), WithRetention(retention))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	// A no-op claim creates a fence row for an ephemeral partition.
	if _, err := s.Claim(ctx, "SESSION#sess-ephemeral", token, 10); err != nil {
		t.Fatalf("noop claim: %v", err)
	}
	if got := countRows(t, s, "outbox_partition_fence"); got != 1 {
		t.Fatalf("expected 1 fence row, got %d", got)
	}

	// A record that expires at t0+1m.
	if err := s.Persist(ctx, []*persistence.OutboxRecord{
		mustRecordAt(t, "re", "sess-exp", t0, t0.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Mark it expired, then jump past 31 days so both the expired row
	// (terminal since t0+1m) and the untouched fence (30d floor) are stale.
	if _, err := s.Expire(ctx, t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("expire: %v", err)
	}
	clk.Advance(31 * 24 * time.Hour)
	if _, err := s.Expire(ctx, clk.Now()); err != nil {
		t.Fatalf("expire trigger: %v", err)
	}

	if got := countRows(t, s, "outbox"); got != 0 {
		t.Fatalf("expected expired row compacted, rows=%d", got)
	}
	if got := countRows(t, s, "outbox_partition_fence"); got != 0 {
		t.Fatalf("expected stale fence compacted, fences=%d", got)
	}
}

// WithRetention(<=0) disables compaction entirely: terminal rows and fences
// are never deleted (the historical behaviour).
func TestRetentionDisabledKeepsTerminalRows(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clk := clocktest.NewAt(t0)

	s, err := NewStore(":memory:", WithClock(clk), WithRetention(0))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	if err := s.Persist(ctx, []*persistence.OutboxRecord{mustRecordAt(t, "rk", "sess-keep", t0, time.Time{})}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := s.Claim(ctx, "SESSION#sess-keep", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.Complete(ctx, []string{"rk"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}

	clk.Advance(365 * 24 * time.Hour)
	if _, err := s.Expire(ctx, clk.Now()); err != nil {
		t.Fatalf("expire trigger: %v", err)
	}

	if got := countRows(t, s, "outbox"); got != 1 {
		t.Fatalf("retention disabled: expected completed row kept, rows=%d", got)
	}
	if got := countRows(t, s, "outbox_partition_fence"); got != 1 {
		t.Fatalf("retention disabled: expected fence kept, fences=%d", got)
	}
}

// A database created before the seq column and fence updated_at existed is
// migrated in place: legacy rows read back with Seq 0 and sort before newer
// rows (their created_at is older); new inserts continue the sequence.
func TestLegacySchemaMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/legacy.db"

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
		 VALUES ('old-1', 'SESSION#sess-mig', 'route-1', 'env-old-1', 'bind-old-1', 'sess-mig', '{"id":"env-old-1","subject":"s"}', ?)`,
		t0.UnixMilli(),
	); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	if _, err := legacy.Exec(
		`INSERT INTO outbox_partition_fence (partition_key, max_version) VALUES ('SESSION#sess-mig', 3)`,
	); err != nil {
		t.Fatalf("legacy fence insert: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	clk := clocktest.NewAt(t0.Add(time.Hour))
	s, err := NewStore(dbPath, WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore over legacy db: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()

	// A new record joins the partition; the legacy row must sort first
	// (older created_at) and hydrate with Seq 0.
	if err := s.Persist(ctx, []*persistence.OutboxRecord{
		mustRecordAt(t, "new-1", "sess-mig", clk.Now(), time.Time{}),
	}); err != nil {
		t.Fatalf("persist new: %v", err)
	}

	pending, err := s.QueryPending(ctx, "SESSION#sess-mig", 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending after migration, got %d", len(pending))
	}
	if pending[0].ID() != "old-1" || pending[0].Seq() != 0 {
		t.Fatalf("legacy row first with Seq 0: got id=%q seq=%d", pending[0].ID(), pending[0].Seq())
	}
	if pending[1].ID() != "new-1" || pending[1].Seq() == 0 {
		t.Fatalf("new row must carry a store-assigned seq: got id=%q seq=%d", pending[1].ID(), pending[1].Seq())
	}

	// The migrated fence must still enforce its high-water-mark (v3), and
	// its legacy row must have been stamped so compaction does not drop it.
	stale, err := s.Claim(ctx, "SESSION#sess-mig", persistence.LeaseToken{Version: 2, Owner: "old-owner"}, 10)
	if err == nil && len(stale) != 0 {
		t.Fatalf("stale token claimed %d records through migrated fence", len(stale))
	}
	var updatedAt int64
	if err := s.sess.db.QueryRow(
		"SELECT updated_at FROM outbox_partition_fence WHERE partition_key = 'SESSION#sess-mig'",
	).Scan(&updatedAt); err != nil {
		t.Fatalf("select fence updated_at: %v", err)
	}
	if updatedAt == 0 {
		t.Fatal("legacy fence row must be stamped with a non-zero updated_at at migration")
	}
}

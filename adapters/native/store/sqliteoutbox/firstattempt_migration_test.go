package sqliteoutbox

// Internal migration test for the replay-budget first_attempted_at column. It
// lives in the production package so it can inspect the raw table schema
// (PRAGMA table_info) through the session handle — proving migrateColumn adds
// the column to a pre-replay-budget database and that the claim-path CASE-WHEN
// stamps it on a migrated row.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// preReplayBudgetSchema is the outbox DDL exactly as it stood BEFORE this work
// package: it already carries claimed_at and seq but has NO first_attempted_at
// column. Opening a database created with it must migrate the column in place.
const preReplayBudgetSchema = `
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
    claimed_at     INTEGER NOT NULL DEFAULT 0,
    replay_count   INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL DEFAULT 0,
    completed_at   INTEGER NOT NULL DEFAULT 0,
    seq            INTEGER NOT NULL DEFAULT 0,
    UNIQUE(envelope_id, binding_id)
);
CREATE INDEX IF NOT EXISTS idx_outbox_partition_status ON outbox(partition_key, status);
CREATE TABLE outbox_partition_fence (
    partition_key TEXT PRIMARY KEY,
    max_version   INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0
);`

// hasColumnT is a test wrapper over the production hasColumn helper that fails
// the test on any PRAGMA error, so the migration assertions read cleanly.
func hasColumnT(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	got, err := hasColumn(db, table, col)
	if err != nil {
		t.Fatalf("hasColumn(%s.%s): %v", table, col, err)
	}
	return got
}

// TestFirstAttemptColumnMigration proves the schema migration in BOTH
// directions (§4.7):
//
//   - A database created with the pre-replay-budget DDL (no first_attempted_at)
//     is migrated in place: the column is absent before NewStore and present
//     after, and claiming a legacy row stamps first_attempted_at through the
//     claim-path CASE-WHEN.
//   - A fresh database stamps first_attempted_at on the first claim.
func TestFirstAttemptColumnMigration(t *testing.T) {
	t.Run("migrated legacy DB adds column and stamps on claim", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := dir + "/pre-budget.db"

		legacy, err := openRawForTest(dbPath)
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		if _, err := legacy.Exec(preReplayBudgetSchema); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
		t0 := time.Unix(1_700_000_000, 0)
		if _, err := legacy.Exec(
			`INSERT INTO outbox (id, partition_key, route_id, envelope_id, binding_id, session_id, envelope_json, status, created_at)
			 VALUES ('mig-1', 'SESSION#sess-mig-fa', 'route-1', 'env-mig-1', 'bind-mig-1', 'sess-mig-fa', '{"id":"env-mig-1","subject":"s"}', 'pending', ?)`,
			t0.UnixMilli(),
		); err != nil {
			t.Fatalf("legacy insert: %v", err)
		}
		// Direction 1: the pre-migration table must NOT have the column.
		if hasColumnT(t, legacy, "outbox", "first_attempted_at") {
			t.Fatal("precondition failed: legacy DDL already has first_attempted_at")
		}
		if err := legacy.Close(); err != nil {
			t.Fatalf("close legacy: %v", err)
		}

		clk := clocktest.NewAt(t0.Add(time.Hour))
		s, err := NewStore(dbPath, WithClock(clk))
		if err != nil {
			t.Fatalf("NewStore over pre-budget db: %v", err)
		}
		defer func() { _ = s.Close() }()

		// Direction 2: migrateColumn added the column.
		if !hasColumnT(t, s.sess.db, "outbox", "first_attempted_at") {
			t.Fatal("migrateColumn did not add first_attempted_at to the migrated table")
		}

		ctx := context.Background()

		// The migrated legacy row reads back with a zero first attempt.
		pending, err := s.QueryPending(ctx, "SESSION#sess-mig-fa", 10)
		if err != nil {
			t.Fatalf("query pending: %v", err)
		}
		if len(pending) != 1 || pending[0].ID() != "mig-1" {
			t.Fatalf("expected the migrated legacy row, got %d records", len(pending))
		}
		if !pending[0].FirstAttemptedAt().IsZero() {
			t.Fatalf("migrated legacy row must read a zero first attempt, got %v", pending[0].FirstAttemptedAt())
		}

		// Claiming the migrated row stamps first_attempted_at via the CASE-WHEN.
		claimed, err := s.Claim(ctx, "SESSION#sess-mig-fa", persistence.LeaseToken{Version: 1, Owner: "owner-mig"}, 10)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim migrated row: err=%v, len=%d", err, len(claimed))
		}
		if !claimed[0].FirstAttemptedAt().Equal(clk.Now()) {
			t.Fatalf("claim must stamp first_attempted_at on the migrated row: got %v, want %v", claimed[0].FirstAttemptedAt(), clk.Now())
		}
	})

	t.Run("fresh DB stamps on first claim", func(t *testing.T) {
		t0 := time.Unix(1_700_000_000, 0)
		clk := clocktest.NewAt(t0)
		s, err := NewStore(":memory:", WithClock(clk))
		if err != nil {
			t.Fatalf("NewStore(:memory:): %v", err)
		}
		defer func() { _ = s.Close() }()

		ctx := context.Background()
		if err := s.Persist(ctx, []*persistence.OutboxRecord{
			mustRecordAt(t, "fresh-1", "sess-fresh-fa", clk.Now(), time.Time{}),
		}); err != nil {
			t.Fatalf("persist: %v", err)
		}
		claimed, err := s.Claim(ctx, "SESSION#sess-fresh-fa", persistence.LeaseToken{Version: 1, Owner: "owner-fresh"}, 10)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim fresh row: err=%v, len=%d", err, len(claimed))
		}
		if !claimed[0].FirstAttemptedAt().Equal(clk.Now()) {
			t.Fatalf("fresh claim must stamp first_attempted_at: got %v, want %v", claimed[0].FirstAttemptedAt(), clk.Now())
		}
	})
}

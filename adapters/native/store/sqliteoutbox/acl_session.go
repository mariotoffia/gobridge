package sqliteoutbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"

	// Driver registration. The blank import lives in this ACL file so
	// it never leaks into the port-side outbox.go.
	_ "modernc.org/sqlite"
)

// sqlSession is the unexported wrapper around the SDK's *sql.DB
// handle. It is the lifecycle/orchestration half of the SQLite ACL:
// every database/sql call the adapter makes flows through a method on
// this type. The port-side Store holds a *sqlSession by pointer and
// never imports database/sql itself.
type sqlSession struct {
	db *sql.DB
}

// openSession opens (or creates) the database at path, applies the
// durability/concurrency pragmas, and runs the idempotent schema migration.
// nowMs stamps legacy fence rows that predate the updated_at column so they
// age from "now" rather than being compacted immediately.
//
// The connection pool is capped at a single open connection. modernc.org/sqlite
// gives every *sql.Conn its own private database for ":memory:" paths, so a
// pool wider than one would fracture an in-memory store into disjoint copies;
// on a file database it would also invite SQLITE_BUSY between in-process
// goroutines. A single writer connection removes both hazards for these
// single-process stores (I2).
//
// ponytail: single-writer ceiling. Good enough for the single-process
// deployments this store targets; a read-heavy file deployment would upgrade
// to a separate read-only connection pool over the WAL. See I2.
func openSession(path string, nowMs int64) (*sqlSession, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: open", "path", path)
	}

	db.SetMaxOpenConns(1)

	// busy_timeout must be armed BEFORE journal_mode=WAL: converting a
	// rollback-journal file to WAL takes an exclusive lock, and without the
	// timeout that first conversion fails fast with SQLITE_BUSY under
	// concurrent opens of a not-yet-WAL file. Arming it first makes the driver
	// block-and-retry up to the timeout — the retry policy for cross-process
	// contention on a file database, including the initial WAL conversion (I2).
	//
	// synchronous=FULL is pinned explicitly (D1): this outbox is the durable
	// hand-off between an at-least-once receive and an at-least-once send, so a
	// committed record MUST survive an OS/host crash. modernc.org/sqlite
	// currently defaults to FULL, but that is an UNPINNED upstream default; a
	// future driver bump or a DSN override could silently drop it to NORMAL,
	// which under WAL can lose the last committed transaction on power loss.
	// Pinning it makes durability a property of THIS store, not of the driver.
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, wrapErr(err, "sqliteoutbox: pragma", "path", path, "pragma", pragma)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: migrate", "path", path)
	}

	if err := migrateColumn(db, "outbox", "claimed_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: migrate claimed_at", "path", path)
	}
	if err := migrateColumn(db, "outbox", "first_attempted_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: migrate first_attempted_at", "path", path)
	}
	if err := migrateColumn(db, "outbox", "seq", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: migrate seq", "path", path)
	}
	if err := migrateColumn(db, "outbox_partition_fence", "updated_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: migrate fence updated_at", "path", path)
	}

	// Identity migration (H2): drop the legacy GLOBAL UNIQUE(envelope_id,
	// binding_id) in favour of the partition-scoped idx_outbox_identity, then
	// (idempotently) (re)create every index. Runs AFTER the column migrations
	// so the rebuild copies a table that already has all columns.
	if err := migrateOutboxIdentity(db); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: migrate outbox identity", "path", path)
	}
	// Stamp legacy fence rows (updated_at 0, i.e. never touched since the
	// column appeared) with "now" so they age through the full fence
	// retention window instead of being compacted on the next Claim.
	if _, err := db.Exec(
		"UPDATE outbox_partition_fence SET updated_at = ? WHERE updated_at = 0", nowMs,
	); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: stamp legacy fences", "path", path)
	}

	return &sqlSession{db: db}, nil
}

// migrateColumn adds a column to a pre-existing table that predates it (I1).
// CREATE TABLE IF NOT EXISTS in schemaSQL already covers fresh databases;
// this handles upgrade-in-place for older files without dropping data.
// Idempotent: a no-op once the column exists.
func migrateColumn(db *sql.DB, table, column, decl string) error {
	has, err := hasColumn(db, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	ddl := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)
	if _, err := db.Exec(ddl); err != nil {
		// A concurrent first-upgrade open (multi-process shared file) can lose
		// the ALTER race with "duplicate column name": both opens passed the
		// table_info check above, then both ran ALTER. Re-check; if the column
		// now exists the migration is effectively complete, so treat it as
		// success rather than failing NewStore (busy_timeout cannot mask a
		// schema error).
		if got, chkErr := hasColumn(db, table, column); chkErr == nil && got {
			return nil
		}
		return fmt.Errorf("sqliteoutbox: add %s.%s column: %w", table, column, err)
	}
	return nil
}

// hasColumn reports whether the table already has the named column. Used for
// both the pre-ALTER check and the post-ALTER race re-check in migrateColumn.
// PRAGMA table_info auto-releases the single connection when the scan reaches
// the end, so a following ALTER on the same SetMaxOpenConns(1) pool does not
// self-deadlock.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("sqliteoutbox: query table_info(%s): %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("sqliteoutbox: scan table_info(%s): %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// migrateOutboxIdentity converts a database carrying the legacy GLOBAL
// UNIQUE(envelope_id, binding_id) constraint to the partition-scoped
// duplicate-detection identity, then (idempotently) creates every outbox
// index. It is safe to run on every open:
//
//   - Fresh databases (schemaSQL already omits the inline constraint) skip the
//     rebuild and just ensure the indexes exist.
//   - Legacy databases (constraint present inline in the table definition) are
//     rebuilt once via the canonical SQLite table-rebuild dance — SQLite cannot
//     drop an inline table constraint with ALTER — then indexed.
//
// A legacy database provably cannot contain two rows sharing (envelope_id,
// binding_id), so the new partition-scoped UNIQUE index can never fail to
// build over migrated data.
func migrateOutboxIdentity(db *sql.DB) error {
	legacy, err := hasLegacyGlobalUnique(db)
	if err != nil {
		return err
	}
	if legacy {
		if err := rebuildOutboxDropGlobalUnique(db); err != nil {
			return err
		}
	}
	if _, err := db.Exec(outboxIndexSQL); err != nil {
		return fmt.Errorf("sqliteoutbox: create outbox indexes: %w", err)
	}
	return nil
}

// hasLegacyGlobalUnique reports whether the persisted outbox table definition
// still carries the inline global UNIQUE(envelope_id, binding_id) constraint.
// It reads the canonical DDL from sqlite_master and compares with all
// whitespace stripped so formatting differences (an ALTER TABLE ADD COLUMN
// rewrites the stored text) cannot fool the probe.
func hasLegacyGlobalUnique(db *sql.DB) (bool, error) {
	var ddl string
	err := db.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'outbox'",
	).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		// No outbox table yet — schemaSQL should have created it, but treat a
		// missing table as "nothing legacy to migrate" rather than erroring.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqliteoutbox: probe outbox schema: %w", err)
	}
	compact := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		default:
			return r
		}
	}, ddl)
	return strings.Contains(compact, "UNIQUE(envelope_id,binding_id)"), nil
}

// rebuildOutboxDropGlobalUnique performs the canonical SQLite table rebuild
// (https://sqlite.org/lang_altertable.html) to drop the inline global UNIQUE
// constraint: create a constraint-free twin, copy every row, drop the old
// table, rename the twin. The whole dance runs in a single transaction so an
// interrupted open leaves the original table intact and a later open retries.
//
// One-time cost: the single-transaction copy holds the writer lock and roughly
// doubles WAL size for its duration. It runs once, only on a legacy DB, at
// open. On a very large backlogged outbox this is a noticeable startup pause,
// and a second process opening the same file concurrently can exceed the 5s
// busy_timeout and fail NewStore — pre-compact or drain a huge legacy outbox
// before the upgrade if that matters. The store's single-process charter makes
// the concurrent-opener case rare.
func rebuildOutboxDropGlobalUnique(db *sql.DB) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("sqliteoutbox: begin identity rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck

	for _, stmt := range []struct {
		sql  string
		what string
	}{
		{createOutboxRebuildTableSQL, "create rebuild table"},
		{copyIntoOutboxRebuildSQL, "copy rows"},
		{"DROP TABLE outbox", "drop legacy table"},
		{"ALTER TABLE outbox_rebuild RENAME TO outbox", "rename rebuild table"},
	} {
		if _, err := tx.Exec(stmt.sql); err != nil {
			return fmt.Errorf("sqliteoutbox: identity rebuild (%s): %w", stmt.what, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqliteoutbox: commit identity rebuild: %w", err)
	}
	return nil
}

// close releases the underlying *sql.DB.
func (s *sqlSession) close() error {
	if err := s.db.Close(); err != nil {
		return wrapErr(err, "sqliteoutbox: close")
	}
	return nil
}

// persist inserts records under a single transaction with per-record
// idempotency (ports.OutboxStore Persist contract): INSERT OR IGNORE
// skips records whose identity already exists — in the store or earlier
// in the same batch — and shared.ErrDuplicateRecord is returned only
// when EVERY record in the batch was a duplicate (nothing persisted).
func (s *sqlSession) persist(ctx context.Context, records []*persistence.OutboxRecord, clk clock.Clock) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapErr(err, "sqliteoutbox: begin tx", "recordCount", len(records))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, insertOutboxSQL)
	if err != nil {
		return wrapErr(err, "sqliteoutbox: prepare persist", "recordCount", len(records))
	}
	defer func() { _ = stmt.Close() }()

	inserted := 0
	for _, r := range records {
		envJSON, err := json.Marshal(r.Snapshot())
		if err != nil {
			return fmt.Errorf("sqliteoutbox: marshal envelope: %w", err)
		}

		var headersJSON []byte
		if dh := r.DispatchHeaders(); dh != nil {
			headersJSON, err = json.Marshal(dh)
			if err != nil {
				return fmt.Errorf("sqliteoutbox: marshal headers: %w", err)
			}
		}

		createdAt := r.CreatedAt()
		if createdAt.IsZero() {
			createdAt = clk.Now()
		}

		var expiresAtMs int64
		if expiresAt := r.ExpiresAt(); !expiresAt.IsZero() {
			expiresAtMs = expiresAt.UnixMilli()
		}

		pk := partitionKey(r)
		res, err := stmt.ExecContext(ctx,
			r.ID(), pk, r.RouteID(), r.EnvelopeID(), r.BindingID(),
			r.SessionID(), r.Address(), string(envJSON), nullableString(headersJSON),
			createdAt.UnixMilli(), expiresAtMs,
			pk, // seq subselect
		)
		if err != nil {
			return wrapErr(err, "sqliteoutbox: insert",
				"envelopeID", r.EnvelopeID(), "bindingID", r.BindingID())
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}

	if err := tx.Commit(); err != nil {
		return wrapErr(err, "sqliteoutbox: commit persist", "recordCount", len(records))
	}
	if inserted == 0 {
		return shared.ErrDuplicateRecord.
			WithMessage("all records in batch already persisted").
			With("recordCount", len(records))
	}
	return nil
}

// claim selects up to limit claimable IDs and atomically flips them to
// claimed under the supplied owner+version, then hydrates the rows.
//
// now/staleClaim drive the optional time-stale reclaim (I1): when
// staleClaim > 0 a record that is still claimed but whose claimed_at is older
// than now-staleClaim is treated as claimable, recovering a claim stranded by
// a crashed owner. When staleClaim == 0 the store is strictly version-only.
func (s *sqlSession) claim(ctx context.Context, pk string, token persistence.LeaseToken, limit int, now time.Time, staleClaim time.Duration) ([]*persistence.OutboxRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: begin tx claim", "partitionKey", pk)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck

	staleEnabled := staleClaim > 0
	var staleCutoffMs int64
	if staleEnabled {
		staleCutoffMs = now.Add(-staleClaim).UnixMilli()
	}

	// Durable version-monotonic fence. The observed high-water-mark is the max
	// of (a) the durable outbox_partition_fence entry, advanced on EVERY claim
	// incl no-ops, and (b) the highest claim_version on a persisted row, kept
	// for backward-compat with legacy rows that predate the fence table. A
	// token older than this is a preempted owner and cannot win a freshly
	// pending row (matches the memory backend's latestVersion and the
	// ports.OutboxStore contract). The fence upsert below commits in this same
	// tx even when zero rows are claimed, so a no-op higher claim is not
	// forgotten.
	var rowMax, fenceMax uint64
	if err := tx.QueryRowContext(ctx, selectMaxClaimVersionSQL, pk).Scan(&rowMax); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: query max claim version", "partitionKey", pk)
	}
	if err := tx.QueryRowContext(ctx, selectFenceVersionSQL, pk).Scan(&fenceMax); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: query fence version", "partitionKey", pk)
	}
	observed := rowMax
	if fenceMax > observed {
		observed = fenceMax
	}
	if token.Version < observed {
		return nil, shared.ErrStaleFencingToken.
			WithMessage("claim rejected: token version is stale").
			With("givenVersion", token.Version).
			With("latestVersion", observed)
	}
	if _, err := tx.ExecContext(ctx, upsertFenceVersionSQL, pk, token.Version, now.UnixMilli()); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: upsert fence version", "partitionKey", pk)
	}

	// limit <= 0 is a fencing no-op (ports.OutboxStore contract): commit the
	// fence advance, claim nothing.
	if limit <= 0 {
		if err := tx.Commit(); err != nil {
			return nil, wrapErr(err, "sqliteoutbox: commit zero-limit claim fence", "partitionKey", pk)
		}
		return nil, nil
	}

	selArgs := make([]any, 0, 4)
	selArgs = append(selArgs, pk, token.Version)
	if staleEnabled {
		selArgs = append(selArgs, staleCutoffMs)
	}
	selArgs = append(selArgs, limit)

	rows, err := tx.QueryContext(ctx, selectClaimableIDsSQL(staleEnabled), selArgs...)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: query claimable", "partitionKey", pk)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, wrapErr(err, "sqliteoutbox: scan claim id", "partitionKey", pk)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: rows err claim", "partitionKey", pk)
	}

	if len(ids) == 0 {
		// No rows claimed, but the fence upsert above must still commit so a
		// no-op higher-version claim durably advances the high-water-mark.
		if err := tx.Commit(); err != nil {
			return nil, wrapErr(err, "sqliteoutbox: commit no-op claim fence", "partitionKey", pk)
		}
		return nil, nil
	}

	// Bind order mirrors updateClaimSQL: claimed_by, claim_version, claimed_at,
	// first_attempted_at (CASE-WHEN stamp-once "now"), ids..., claim_version
	// (fence guard), [stale_cutoff_ms].
	args := make([]any, 0, len(ids)+6)
	args = append(args, token.Owner, token.Version, now.UnixMilli(), now.UnixMilli())
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, token.Version)
	if staleEnabled {
		args = append(args, staleCutoffMs)
	}

	res, err := tx.ExecContext(ctx, updateClaimSQL(len(ids), staleEnabled), args...)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: update claim",
			"partitionKey", pk, "ownerID", token.Owner, "recordCount", len(ids))
	}
	// The single-writer connection (I2) plus the enclosing transaction means
	// every id selected as claimable is still claimable at UPDATE time, so the
	// guarded UPDATE affects exactly len(ids) rows. A shortfall would signal a
	// broken isolation assumption; fail closed rather than hydrate rows this
	// call did not actually claim.
	if n, _ := res.RowsAffected(); n != int64(len(ids)) {
		return nil, shared.ErrStaleFencingToken.
			WithMessage("claim lost a fenced row between select and update").
			With("partitionKey", pk).
			With("expected", len(ids)).
			With("affected", n)
	}

	// Hydrate INSIDE the claim transaction so the rows returned are exactly
	// the rows this call flipped to claimed — a competing claimer that wins
	// the row after our commit can never leak into this result set (the
	// pre-fix read-after-commit was a thief-visible snapshot; it only
	// converged via fencing).
	recs, err := fetchByIDsTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: commit claim",
			"partitionKey", pk, "ownerID", token.Owner)
	}

	return recs, nil
}

// fetchByIDsTx hydrates records by ID using the canonical column list,
// inside the caller's transaction.
func fetchByIDsTx(ctx context.Context, tx *sql.Tx, ids []string) ([]*persistence.OutboxRecord, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := tx.QueryContext(ctx, selectByIDsSQL(len(ids)), args...)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: fetch by ids", "recordCount", len(ids))
	}
	defer func() { _ = rows.Close() }()

	return scanOutboxRecords(rows)
}

// dedupIDs returns recordIDs with duplicates removed, preserving first-seen
// order. Complete/Release compare RowsAffected against len(ids) as a fence
// check; a duplicate id updates a single physical row once, so leaving
// duplicates in would make the achievable RowsAffected fall below len and
// trip a spurious ErrStaleFencingToken (M4). Collapsing them is loss-free
// because the guarded UPDATE ... WHERE id IN (...) is idempotent per id.
func dedupIDs(ids []string) []string {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// complete marks records as completed iff they are still claimed by
// token.Owner at token.Version. If fewer rows were updated than
// requested the claim has been preempted and ErrStaleFencingToken is
// returned.
func (s *sqlSession) complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken, now time.Time) error {
	recordIDs = dedupIDs(recordIDs)
	if len(recordIDs) == 0 {
		return nil
	}

	args := make([]any, 0, len(recordIDs)+3)
	args = append(args, now.UnixMilli())
	for _, id := range recordIDs {
		args = append(args, id)
	}
	args = append(args, token.Owner, token.Version)

	res, err := s.db.ExecContext(ctx, updateCompleteSQL(len(recordIDs)), args...)
	if err != nil {
		return wrapErr(err, "sqliteoutbox: complete",
			"recordCount", len(recordIDs), "ownerID", token.Owner)
	}

	n, _ := res.RowsAffected()
	if n < int64(len(recordIDs)) {
		return shared.ErrStaleFencingToken.
			WithMessage("claim version mismatch on complete")
	}
	return nil
}

// release returns records from claimed to pending iff they are still
// claimed by token.Owner at token.Version, so a still-alive owner can
// re-claim and retry them after a transient egress failure. If fewer
// rows were updated than requested the claim has been preempted and
// ErrStaleFencingToken is returned. replay_count is left untouched — the
// next Claim increments it, preserving the poison-message cap.
func (s *sqlSession) release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	recordIDs = dedupIDs(recordIDs)
	if len(recordIDs) == 0 {
		return nil
	}

	args := make([]any, 0, len(recordIDs)+2)
	for _, id := range recordIDs {
		args = append(args, id)
	}
	args = append(args, token.Owner, token.Version)

	res, err := s.db.ExecContext(ctx, releaseOutboxSQL(len(recordIDs)), args...)
	if err != nil {
		return wrapErr(err, "sqliteoutbox: release",
			"recordCount", len(recordIDs), "ownerID", token.Owner)
	}

	n, _ := res.RowsAffected()
	if n < int64(len(recordIDs)) {
		return shared.ErrStaleFencingToken.
			WithMessage("claim version mismatch on release")
	}
	return nil
}

// expire flips pending records past their expires_at deadline to
// expired, scoped to partition (M1). Claimed records and records in
// other partitions are left untouched. Returns rows affected.
func (s *sqlSession) expire(ctx context.Context, before time.Time, partition string) (int, error) {
	res, err := s.db.ExecContext(ctx, expireOutboxSQL, partition, before.UnixMilli())
	if err != nil {
		return 0, wrapErr(err, "sqliteoutbox: expire")
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// compact physically deletes terminal rows (completed/expired) older than
// recordCutoff and fence rows untouched since fenceCutoff. Mirrors the
// DynamoDB backend's TTL-based cleanup so the single-file production
// backend does not grow unboundedly (retention contract documented on
// WithRetention).
func (s *sqlSession) compact(ctx context.Context, recordCutoff, fenceCutoff time.Time) error {
	if _, err := s.db.ExecContext(ctx, deleteCompletedSQL, recordCutoff.UnixMilli()); err != nil {
		return wrapErr(err, "sqliteoutbox: compact completed")
	}
	if _, err := s.db.ExecContext(ctx, deleteExpiredSQL, recordCutoff.UnixMilli()); err != nil {
		return wrapErr(err, "sqliteoutbox: compact expired")
	}
	if _, err := s.db.ExecContext(ctx, deleteStaleFencesSQL, fenceCutoff.UnixMilli()); err != nil {
		return wrapErr(err, "sqliteoutbox: compact fences")
	}
	return nil
}

// queryPending returns up to limit pending records under partition pk.
func (s *sqlSession) queryPending(ctx context.Context, pk string, limit int) ([]*persistence.OutboxRecord, error) {
	rows, err := s.db.QueryContext(ctx, selectPendingByPartitionSQL, pk, limit)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: query pending", "partitionKey", pk)
	}
	defer func() { _ = rows.Close() }()

	return scanOutboxRecords(rows)
}

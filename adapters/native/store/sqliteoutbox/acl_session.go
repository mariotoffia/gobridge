package sqliteoutbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
// durability/concurrency pragmas, and creates the schema.
//
// There is no in-place schema migration. GoBridge has never been deployed, so
// no database written by an earlier build exists: the DDL is the whole story,
// and CREATE TABLE/INDEX IF NOT EXISTS makes opening an existing file of the
// CURRENT schema a no-op. A future schema change ships as a new DDL plus a
// migration only once there is deployed data to migrate.
//
// The connection pool is capped at a single open connection. modernc.org/sqlite
// gives every *sql.Conn its own private database for ":memory:" paths, so a
// pool wider than one would fracture an in-memory store into disjoint copies;
// on a file database it would also invite SQLITE_BUSY between in-process
// goroutines. A single writer connection removes both hazards for these
// single-process stores.
//
// ponytail: single-writer ceiling. Good enough for the single-process
// deployments this store targets; a read-heavy file deployment would upgrade
// to a separate read-only connection pool over the WAL. See.
func openSession(path string) (*sqlSession, error) {
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
	// contention on a file database, including the initial WAL conversion.
	//
	// synchronous=FULL is pinned explicitly: this outbox is the durable
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
		return nil, wrapErr(err, "sqliteoutbox: create schema", "path", path)
	}
	if _, err := db.Exec(outboxIndexSQL); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: create indexes", "path", path)
	}

	return &sqlSession{db: db}, nil
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
			orderingKeyOf(r),
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
// now/staleClaim drive the optional time-stale reclaim: when
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
	// incl no-ops, and (b) the highest claim_version stamped on a persisted row,
	// which recovers the mark when retention compaction has dropped the fence
	// entry of a partition that still holds claimed records. A token older than
	// this is a preempted owner and cannot win a freshly pending row (matches
	// the memory backend's latestVersion and the ports.OutboxStore contract).
	// The fence upsert below commits in this same tx even when zero rows are
	// claimed, so a no-op higher claim is not forgotten.
	observed, err := partitionFenceVersion(ctx, tx, pk)
	if err != nil {
		return nil, err
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

	// Bind order mirrors selectClaimableIDsSQL: the claimable predicate appears
	// twice — once for the candidate row, once inside the head-of-line subquery
	// that tests the candidate's older same-key siblings.
	selArgs := make([]any, 0, 6)
	selArgs = append(selArgs, pk, token.Version)
	if staleEnabled {
		selArgs = append(selArgs, staleCutoffMs)
	}
	selArgs = append(selArgs, token.Version)
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
	// The single-writer connection plus the enclosing transaction means
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
// trip a spurious ErrStaleFencingToken. Collapsing them is loss-free
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
// expired, scoped to partition. Claimed records and records in
// other partitions are left untouched. Returns rows affected.
//
// The sweep is lease-fenced against the same durable high-water-mark as claim,
// inside ONE transaction: read the fence, reject a stale token, raise the fence
// to this token, then flip the rows. A takeover that raises the fence
// concurrently therefore either lands entirely before this tx (and this stale
// token is rejected) or entirely after it — it can never interleave with the
// destructive UPDATE.
func (s *sqlSession) expire(
	ctx context.Context,
	before time.Time,
	partition string,
	token persistence.LeaseToken,
	now time.Time,
) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, wrapErr(err, "sqliteoutbox: begin tx expire", "partitionKey", partition)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck

	observed, err := partitionFenceVersion(ctx, tx, partition)
	if err != nil {
		return 0, err
	}
	if token.Version < observed {
		return 0, shared.ErrStaleFencingToken.
			WithMessage("expire rejected: token version is stale").
			With("givenVersion", token.Version).
			With("latestVersion", observed)
	}
	if _, err := tx.ExecContext(ctx, upsertFenceVersionSQL, partition, token.Version, now.UnixMilli()); err != nil {
		return 0, wrapErr(err, "sqliteoutbox: upsert fence version on expire", "partitionKey", partition)
	}

	res, err := tx.ExecContext(ctx, expireOutboxSQL, partition, before.UnixMilli())
	if err != nil {
		return 0, wrapErr(err, "sqliteoutbox: expire")
	}
	n, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, wrapErr(err, "sqliteoutbox: commit expire", "partitionKey", partition)
	}
	return int(n), nil
}

// partitionFenceVersion reads the partition's fencing high-water-mark inside tx:
// the max of the durable outbox_partition_fence entry and the highest
// claim_version stamped on a persisted row (the latter recovers the mark when
// retention compaction has dropped the fence entry of a partition that still
// holds claimed records). Shared by claim and expire so the two can never drift
// apart on what "stale" means.
func partitionFenceVersion(ctx context.Context, tx *sql.Tx, pk string) (uint64, error) {
	var rowMax, fenceMax uint64
	if err := tx.QueryRowContext(ctx, selectMaxClaimVersionSQL, pk).Scan(&rowMax); err != nil {
		return 0, wrapErr(err, "sqliteoutbox: query max claim version", "partitionKey", pk)
	}
	if err := tx.QueryRowContext(ctx, selectFenceVersionSQL, pk).Scan(&fenceMax); err != nil {
		return 0, wrapErr(err, "sqliteoutbox: query fence version", "partitionKey", pk)
	}
	if fenceMax > rowMax {
		return fenceMax, nil
	}
	return rowMax, nil
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

// countPending returns the number of pending records under partition pk, or
// across all partitions when pk is empty. Backed by the COUNT(*) queries in
// acl_query.go (idx_outbox_partition_status), so it is an index scan, never a
// full row materialisation — the efficient backlog primitive behind
// ports.OutboxDepthReporter. A real read failure is wrapped and returned as-is.
func (s *sqlSession) countPending(ctx context.Context, pk string) (int, error) {
	var n int
	if pk == "" {
		if err := s.db.QueryRowContext(ctx, countPendingAllSQL).Scan(&n); err != nil {
			return 0, wrapErr(err, "sqliteoutbox: count pending all")
		}
		return n, nil
	}
	if err := s.db.QueryRowContext(ctx, countPendingByPartitionSQL, pk).Scan(&n); err != nil {
		return 0, wrapErr(err, "sqliteoutbox: count pending", "partitionKey", pk)
	}
	return n, nil
}

// countClaimed returns the number of CLAIMED records under partition pk, or
// across all partitions when pk is empty — work an owner took but has not
// driven to a terminal state (ports.OutboxClaimedDepthReporter). Backed by the
// same partition/status index as countPending. A real read failure is wrapped
// and returned as-is.
func (s *sqlSession) countClaimed(ctx context.Context, pk string) (int, error) {
	var n int
	if pk == "" {
		if err := s.db.QueryRowContext(ctx, countClaimedAllSQL).Scan(&n); err != nil {
			return 0, wrapErr(err, "sqliteoutbox: count claimed all")
		}
		return n, nil
	}
	if err := s.db.QueryRowContext(ctx, countClaimedByPartitionSQL, pk).Scan(&n); err != nil {
		return 0, wrapErr(err, "sqliteoutbox: count claimed", "partitionKey", pk)
	}
	return n, nil
}

// orderingKeyOf returns the record's ordering key, or "" when it has none. It
// reads the aggregate accessor rather than a persistence snapshot: the snapshot
// deep-copies the envelope, which would put a full clone per record on the
// Persist hot path just to read one header.
func orderingKeyOf(r *persistence.OutboxRecord) string {
	key, ok := r.OrderingKey()
	if !ok {
		return ""
	}
	return key
}

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

// openSession opens (or creates) the database at path, applies WAL
// pragma, and runs the idempotent schema migration.
func openSession(path string) (*sqlSession, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: open", "path", path)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: pragma", "path", path)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: migrate", "path", path)
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

// persist inserts records under a single transaction. Duplicate
// records are translated to shared.ErrDuplicateRecord at the SDK
// boundary.
func (s *sqlSession) persist(ctx context.Context, records []*persistence.OutboxRecord, clk clock.Clock) error {
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

		res, err := stmt.ExecContext(ctx,
			r.ID(), partitionKey(r), r.RouteID(), r.EnvelopeID(), r.BindingID(),
			r.SessionID(), r.Address(), string(envJSON), nullableString(headersJSON),
			createdAt.UnixMilli(), expiresAtMs,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return shared.ErrDuplicateRecord.
					WithMessage("duplicate outbox record").
					With("envelopeID", r.EnvelopeID()).
					With("bindingID", r.BindingID())
			}
			return wrapErr(err, "sqliteoutbox: insert",
				"envelopeID", r.EnvelopeID(), "bindingID", r.BindingID())
		}

		n, _ := res.RowsAffected()
		if n == 0 {
			return shared.ErrDuplicateRecord.
				WithMessage("duplicate outbox record").
				With("envelopeID", r.EnvelopeID()).
				With("bindingID", r.BindingID())
		}
	}

	if err := tx.Commit(); err != nil {
		return wrapErr(err, "sqliteoutbox: commit persist", "recordCount", len(records))
	}
	return nil
}

// claim selects up to limit claimable IDs and atomically flips them to
// claimed under the supplied owner+version, then hydrates the rows.
func (s *sqlSession) claim(ctx context.Context, pk string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: begin tx claim", "partitionKey", pk)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck

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
	if _, err := tx.ExecContext(ctx, upsertFenceVersionSQL, pk, token.Version); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: upsert fence version", "partitionKey", pk)
	}

	rows, err := tx.QueryContext(ctx, selectClaimableIDsSQL, pk, token.Version, limit)
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

	args := make([]any, 0, len(ids)+2)
	args = append(args, token.Owner, token.Version)
	for _, id := range ids {
		args = append(args, id)
	}

	if _, err := tx.ExecContext(ctx, updateClaimSQL(len(ids)), args...); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: update claim",
			"partitionKey", pk, "ownerID", token.Owner, "recordCount", len(ids))
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: commit claim",
			"partitionKey", pk, "ownerID", token.Owner)
	}

	return s.fetchByIDs(ctx, ids)
}

// fetchByIDs hydrates records by ID using the canonical column list.
func (s *sqlSession) fetchByIDs(ctx context.Context, ids []string) ([]*persistence.OutboxRecord, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := s.db.QueryContext(ctx, selectByIDsSQL(len(ids)), args...)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: fetch by ids", "recordCount", len(ids))
	}
	defer func() { _ = rows.Close() }()

	return scanOutboxRecords(rows)
}

// complete marks records as completed iff they are still claimed by
// token.Owner at token.Version. If fewer rows were updated than
// requested the claim has been preempted and ErrStaleFencingToken is
// returned.
func (s *sqlSession) complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken, now time.Time) error {
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

// expire flips pending records past their expires_at deadline to
// expired. Claimed records are left untouched. Returns rows affected.
func (s *sqlSession) expire(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, expireOutboxSQL, before.UnixMilli())
	if err != nil {
		return 0, wrapErr(err, "sqliteoutbox: expire")
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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

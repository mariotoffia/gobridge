package sqliteoutbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"

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
// records are translated to domain.ErrDuplicateRecord at the SDK
// boundary.
func (s *sqlSession) persist(ctx context.Context, records []domain.OutboxRecord, clk clock.Clock) error {
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

	for i := range records {
		r := &records[i]
		envJSON, err := json.Marshal(r.Envelope)
		if err != nil {
			return fmt.Errorf("sqliteoutbox: marshal envelope: %w", err)
		}

		var headersJSON []byte
		if r.DispatchHeaders != nil {
			headersJSON, err = json.Marshal(r.DispatchHeaders)
			if err != nil {
				return fmt.Errorf("sqliteoutbox: marshal headers: %w", err)
			}
		}

		createdAt := r.CreatedAt
		if createdAt.IsZero() {
			createdAt = clk.Now()
		}

		var expiresAtMs int64
		if !r.ExpiresAt.IsZero() {
			expiresAtMs = r.ExpiresAt.UnixMilli()
		}

		res, err := stmt.ExecContext(ctx,
			r.ID, partitionKey(r), r.RouteID, r.EnvelopeID, r.BindingID,
			r.SessionID, r.Address, string(envJSON), nullableString(headersJSON),
			createdAt.UnixMilli(), expiresAtMs,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrDuplicateRecord.
					WithMessage("duplicate outbox record").
					With("envelopeID", r.EnvelopeID).
					With("bindingID", r.BindingID)
			}
			return wrapErr(err, "sqliteoutbox: insert",
				"envelopeID", r.EnvelopeID, "bindingID", r.BindingID)
		}

		n, _ := res.RowsAffected()
		if n == 0 {
			return domain.ErrDuplicateRecord.
				WithMessage("duplicate outbox record").
				With("envelopeID", r.EnvelopeID).
				With("bindingID", r.BindingID)
		}
	}

	if err := tx.Commit(); err != nil {
		return wrapErr(err, "sqliteoutbox: commit persist", "recordCount", len(records))
	}
	return nil
}

// claim selects up to limit claimable IDs and atomically flips them to
// claimed under the supplied owner+version, then hydrates the rows.
func (s *sqlSession) claim(ctx context.Context, pk string, ownerID string, token domain.LeaseToken, limit int) ([]domain.OutboxRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: begin tx claim", "partitionKey", pk)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck

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
		return nil, nil
	}

	args := make([]any, 0, len(ids)+2)
	args = append(args, ownerID, token.Version)
	for _, id := range ids {
		args = append(args, id)
	}

	if _, err := tx.ExecContext(ctx, updateClaimSQL(len(ids)), args...); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: update claim",
			"partitionKey", pk, "ownerID", ownerID, "recordCount", len(ids))
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: commit claim",
			"partitionKey", pk, "ownerID", ownerID)
	}

	return s.fetchByIDs(ctx, ids)
}

// fetchByIDs hydrates records by ID using the canonical column list.
func (s *sqlSession) fetchByIDs(ctx context.Context, ids []string) ([]domain.OutboxRecord, error) {
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

// complete marks records as completed iff their claim_version still
// matches token.Version. If fewer rows were updated than requested
// the claim has been preempted and ErrStaleFencingToken is returned.
func (s *sqlSession) complete(ctx context.Context, recordIDs []string, token domain.LeaseToken, now time.Time) error {
	if len(recordIDs) == 0 {
		return nil
	}

	args := make([]any, 0, len(recordIDs)+2)
	args = append(args, now.UnixMilli())
	for _, id := range recordIDs {
		args = append(args, id)
	}
	args = append(args, token.Version)

	res, err := s.db.ExecContext(ctx, updateCompleteSQL(len(recordIDs)), args...)
	if err != nil {
		return wrapErr(err, "sqliteoutbox: complete",
			"recordCount", len(recordIDs), "ownerID", token.Owner)
	}

	n, _ := res.RowsAffected()
	if n < int64(len(recordIDs)) {
		return domain.ErrStaleFencingToken.
			WithMessage("claim version mismatch on complete")
	}
	return nil
}

// expire flips pending/claimed records past their expires_at deadline
// to expired. Returns the number of rows affected.
func (s *sqlSession) expire(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, expireOutboxSQL, before.UnixMilli())
	if err != nil {
		return 0, wrapErr(err, "sqliteoutbox: expire")
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// queryPending returns up to limit pending records under partition pk.
func (s *sqlSession) queryPending(ctx context.Context, pk string, limit int) ([]domain.OutboxRecord, error) {
	rows, err := s.db.QueryContext(ctx, selectPendingByPartitionSQL, pk, limit)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: query pending", "partitionKey", pk)
	}
	defer func() { _ = rows.Close() }()

	return scanOutboxRecords(rows)
}

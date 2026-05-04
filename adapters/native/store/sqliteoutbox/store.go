package sqliteoutbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/logging"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS outbox (
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
CREATE INDEX IF NOT EXISTS idx_outbox_partition_status ON outbox(partition_key, status);
`

// Store implements ports.OutboxStore using SQLite for local durable
// persistence in tests and single-process deployments.
type Store struct {
	db     *sql.DB
	clk    clock.Clock
	logger *slog.Logger
}

// Option configures a Store.
type Option func(*Store)

// WithClock overrides the clock used for timestamps.
// Defaults to clock.System when nil or not set.
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithLogger sets a structured logger for trace-level diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// NewStore opens (or creates) a SQLite database at dbPath and runs the
// schema migration. Use ":memory:" for a purely in-memory database.
func NewStore(dbPath string, opts ...Option) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: open", "path", dbPath)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: pragma", "path", dbPath)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, wrapErr(err, "sqliteoutbox: migrate", "path", dbPath)
	}

	s := &Store{db: db, clk: clock.System}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return wrapErr(err, "sqliteoutbox: close")
	}
	return nil
}

func partitionKey(r *domain.OutboxRecord) string {
	if r.SessionID != "" {
		return "SESSION#" + r.SessionID
	}
	return "BINDING#" + r.BindingID
}

func (s *Store) Persist(ctx context.Context, records []domain.OutboxRecord) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: persist", "count", len(records))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapErr(err, "sqliteoutbox: begin tx", "recordCount", len(records))
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO outbox (id, partition_key, route_id, envelope_id, binding_id,
		 session_id, address, envelope_json, headers_json, status, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`)
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
			createdAt = s.clk.Now()
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

func (s *Store) Claim(ctx context.Context, pk string, ownerID string, token domain.LeaseToken, limit int) ([]domain.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: claim", "partition_key", pk, "limit", limit)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: begin tx claim", "partitionKey", pk)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM outbox
		 WHERE partition_key = ? AND (status = 'pending' OR (status = 'claimed' AND claim_version < ?))
		 ORDER BY created_at
		 LIMIT ?`,
		pk, token.Version, limit,
	)
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

	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+3)
	args = append(args, ownerID, token.Version)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(
			`UPDATE outbox SET status = 'claimed', claimed_by = ?, claim_version = ?,
			 replay_count = replay_count + 1
			 WHERE id IN (%s)`, strings.Join(placeholders, ",")),
		args...,
	)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: update claim",
			"partitionKey", pk, "ownerID", ownerID, "recordCount", len(ids))
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: commit claim",
			"partitionKey", pk, "ownerID", ownerID)
	}

	return s.fetchByIDs(ctx, ids)
}

func (s *Store) Complete(ctx context.Context, recordIDs []string, token domain.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: complete", "count", len(recordIDs))
	}

	if len(recordIDs) == 0 {
		return nil
	}

	placeholders := make([]string, len(recordIDs))
	args := make([]any, 0, len(recordIDs)+2)
	args = append(args, s.clk.Now().UnixMilli())
	for i, id := range recordIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, token.Version)

	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(
			`UPDATE outbox SET status = 'completed', completed_at = ?
			 WHERE id IN (%s) AND claim_version = ?`,
			strings.Join(placeholders, ",")),
		args...,
	)
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

func (s *Store) Expire(ctx context.Context, before time.Time) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: expire")
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE outbox SET status = 'expired'
		 WHERE expires_at > 0 AND expires_at < ? AND status IN ('pending', 'claimed')`,
		before.UnixMilli(),
	)
	if err != nil {
		return 0, wrapErr(err, "sqliteoutbox: expire")
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) QueryPending(ctx context.Context, pk string, limit int) ([]domain.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: query_pending", "partition_key", pk, "limit", limit)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, partition_key, route_id, envelope_id, binding_id, session_id,
		        address, envelope_json, headers_json, status, claimed_by, claim_version,
		        replay_count, created_at, expires_at, completed_at
		 FROM outbox
		 WHERE partition_key = ? AND status = 'pending'
		 ORDER BY created_at
		 LIMIT ?`,
		pk, limit,
	)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: query pending", "partitionKey", pk)
	}
	defer func() { _ = rows.Close() }()

	return scanRecords(rows)
}

// --- helpers ---

func (s *Store) fetchByIDs(ctx context.Context, ids []string) ([]domain.OutboxRecord, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(
			`SELECT id, partition_key, route_id, envelope_id, binding_id, session_id,
			        address, envelope_json, headers_json, status, claimed_by, claim_version,
			        replay_count, created_at, expires_at, completed_at
			 FROM outbox WHERE id IN (%s) ORDER BY created_at`,
			strings.Join(placeholders, ",")),
		args...,
	)
	if err != nil {
		return nil, wrapErr(err, "sqliteoutbox: fetch by ids", "recordCount", len(ids))
	}
	defer func() { _ = rows.Close() }()

	return scanRecords(rows)
}

func scanRecords(rows *sql.Rows) ([]domain.OutboxRecord, error) {
	var result []domain.OutboxRecord
	for rows.Next() {
		var (
			r           domain.OutboxRecord
			pk          string
			envJSON     string
			headersJSON sql.NullString
			status      string
			createdAtMs int64
			expiresAtMs int64
			completedMs int64
		)
		err := rows.Scan(
			&r.ID, &pk, &r.RouteID, &r.EnvelopeID, &r.BindingID, &r.SessionID,
			&r.Address, &envJSON, &headersJSON, &status, &r.ClaimedBy, &r.ClaimVersion,
			&r.ReplayCount, &createdAtMs, &expiresAtMs, &completedMs,
		)
		if err != nil {
			return nil, wrapErr(err, "sqliteoutbox: scan record")
		}

		r.Status = domain.OutboxStatus(status)
		r.CreatedAt = time.UnixMilli(createdAtMs)
		if expiresAtMs > 0 {
			r.ExpiresAt = time.UnixMilli(expiresAtMs)
		}
		if completedMs > 0 {
			r.CompletedAt = time.UnixMilli(completedMs)
		}

		if err := json.Unmarshal([]byte(envJSON), &r.Envelope); err != nil {
			return nil, fmt.Errorf("sqliteoutbox: unmarshal envelope: %w", err)
		}
		if headersJSON.Valid && headersJSON.String != "" {
			if err := json.Unmarshal([]byte(headersJSON.String), &r.DispatchHeaders); err != nil {
				return nil, fmt.Errorf("sqliteoutbox: unmarshal headers: %w", err)
			}
		}

		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: rows err scan")
	}
	return result, nil
}

func nullableString(b []byte) sql.NullString {
	if b == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

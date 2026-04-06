package sqlitedlq

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/domain"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS dlq (
    id              TEXT PRIMARY KEY,
    route_id        TEXT NOT NULL,
    binding_id      TEXT NOT NULL DEFAULT '',
    session_id      TEXT NOT NULL DEFAULT '',
    source_id       TEXT NOT NULL DEFAULT '',
    correlation_id  TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT '',
    error_code      TEXT NOT NULL DEFAULT '',
    last_error      TEXT NOT NULL DEFAULT '',
    envelope_json   TEXT NOT NULL,
    failed_at       INTEGER NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    replayed        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_dlq_route_id ON dlq(route_id);
CREATE INDEX IF NOT EXISTS idx_dlq_category ON dlq(category);
CREATE INDEX IF NOT EXISTS idx_dlq_failed_at ON dlq(failed_at);
`

// Store implements ports.DLQStore using SQLite for local durable
// persistence in tests and single-process deployments.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
}

// SetLogger sets a structured logger for trace-level diagnostics.
func (s *Store) SetLogger(l *slog.Logger) { s.logger = l }

// NewStore opens (or creates) a SQLite database at dbPath and runs the
// schema migration. Use ":memory:" for a purely in-memory database.
func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("sqlitedlq: open: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitedlq: pragma: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitedlq: migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Write(ctx context.Context, entry domain.DLQEntry) error {
	logging.TraceContext(s.logger, ctx, "sqlitedlq: write",
		"route_id", entry.RouteID, "entry_id", entry.ID)
	envJSON, err := json.Marshal(entry.Envelope)
	if err != nil {
		return fmt.Errorf("sqlitedlq: marshal envelope: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO dlq (id, route_id, binding_id, session_id, source_id,
		 correlation_id, reason, category, error_code, last_error,
		 envelope_json, failed_at, attempts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.RouteID, entry.BindingID, entry.SessionID, entry.SourceID,
		entry.CorrelationID, entry.Reason, entry.Category, entry.ErrorCode, entry.LastError,
		string(envJSON), entry.FailedAt.UnixMilli(), entry.Attempts,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrDuplicateRecord.With("entryID", entry.ID)
		}
		return fmt.Errorf("sqlitedlq: insert: %w", err)
	}

	return nil
}

func (s *Store) List(ctx context.Context, filter domain.DLQFilter) ([]domain.DLQEntry, error) {
	logging.TraceContext(s.logger, ctx, "sqlitedlq: list",
		"route_id", filter.RouteID, "limit", filter.Limit)
	var clauses []string
	var args []any

	if filter.RouteID != "" {
		clauses = append(clauses, "route_id = ?")
		args = append(args, filter.RouteID)
	}
	if filter.Category != "" {
		clauses = append(clauses, "category = ?")
		args = append(args, filter.Category)
	}
	if !filter.Since.IsZero() {
		clauses = append(clauses, "failed_at >= ?")
		args = append(args, filter.Since.UnixMilli())
	}
	if !filter.Before.IsZero() {
		clauses = append(clauses, "failed_at < ?")
		args = append(args, filter.Before.UnixMilli())
	}

	query := "SELECT id, route_id, binding_id, session_id, source_id, correlation_id, reason, category, error_code, last_error, envelope_json, failed_at, attempts FROM dlq"
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY failed_at DESC"

	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlitedlq: list: %w", err)
	}
	defer rows.Close()

	return scanEntries(rows)
}

func (s *Store) Replay(ctx context.Context, entryIDs []string) error {
	if len(entryIDs) == 0 {
		return nil
	}

	placeholders := make([]string, len(entryIDs))
	args := make([]any, 0, len(entryIDs)+1)
	args = append(args, time.Now().UnixMilli())
	for i, id := range entryIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(
			`UPDATE dlq SET replayed = ? WHERE id IN (%s) AND replayed = 0`,
			strings.Join(placeholders, ",")),
		args...,
	)
	if err != nil {
		return fmt.Errorf("sqlitedlq: replay: %w", err)
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound.WithMessage("no unreplayed DLQ entries matched")
	}

	return nil
}

func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	logging.TraceContext(s.logger, ctx, "sqlitedlq: purge",
		"before", before)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM dlq WHERE failed_at < ?`,
		before.UnixMilli(),
	)
	if err != nil {
		return 0, fmt.Errorf("sqlitedlq: purge: %w", err)
	}

	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- helpers ---

func scanEntries(rows *sql.Rows) ([]domain.DLQEntry, error) {
	result := make([]domain.DLQEntry, 0)
	for rows.Next() {
		var (
			e          domain.DLQEntry
			envJSON    string
			failedAtMs int64
		)
		err := rows.Scan(
			&e.ID, &e.RouteID, &e.BindingID, &e.SessionID, &e.SourceID,
			&e.CorrelationID, &e.Reason, &e.Category, &e.ErrorCode, &e.LastError,
			&envJSON, &failedAtMs, &e.Attempts,
		)
		if err != nil {
			return nil, fmt.Errorf("sqlitedlq: scan: %w", err)
		}

		e.FailedAt = time.UnixMilli(failedAtMs)

		if err := json.Unmarshal([]byte(envJSON), &e.Envelope); err != nil {
			return nil, fmt.Errorf("sqlitedlq: unmarshal envelope: %w", err)
		}

		result = append(result, e)
	}

	return result, rows.Err()
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

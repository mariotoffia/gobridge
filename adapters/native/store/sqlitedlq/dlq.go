package sqlitedlq

import (
	"context"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/logging"
)

// Store implements ports.DLQStore using SQLite for local durable
// persistence in tests and single-process deployments.
//
// All SDK interaction (database/sql, modernc.org/sqlite) is confined
// to the acl_*.go files in this package; this file is the port-side
// implementation and intentionally imports no driver packages.
type Store struct {
	sess   *sqlSession
	logger *slog.Logger
}

// SetLogger sets a structured logger for trace-level diagnostics.
func (s *Store) SetLogger(l *slog.Logger) { s.logger = l }

// NewStore opens (or creates) a SQLite database at dbPath and runs the
// schema migration. Use ":memory:" for a purely in-memory database.
func NewStore(dbPath string) (*Store, error) {
	sess, err := openSession(dbPath)
	if err != nil {
		return nil, err
	}
	return &Store{sess: sess}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.sess.close()
}

// Write inserts a single DLQ entry. Duplicates surface as
// shared.ErrDuplicateRecord.
func (s *Store) Write(ctx context.Context, entry routing.DLQEntry) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: write",
			"route_id", entry.RouteID(), "entry_id", entry.ID())
	}
	return s.sess.write(ctx, entry)
}

// List returns DLQ entries matching the supplied filter, newest first.
func (s *Store) List(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: list",
			"route_id", filter.RouteID, "limit", filter.Limit)
	}
	return s.sess.list(ctx, filter)
}

// Get returns the entry with id or shared.ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (routing.DLQEntry, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: get", "entry_id", id)
	}
	return s.sess.get(ctx, id)
}

// Delete removes the entries with the supplied ids and returns the
// number actually deleted.
func (s *Store) Delete(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: delete", "count", len(ids))
	}
	return s.sess.delete(ctx, ids)
}

// DeleteByFilter removes every entry matching the filter.
func (s *Store) DeleteByFilter(ctx context.Context, filter routing.DLQFilter) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: delete_by_filter",
			"route_id", filter.RouteID, "category", filter.Category)
	}
	return s.sess.deleteByFilter(ctx, filter)
}

// Purge deletes every entry whose failed_at is strictly before the
// supplied cutoff. Returns the number of entries deleted.
func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: purge", "before", before)
	}
	return s.sess.purge(ctx, before)
}

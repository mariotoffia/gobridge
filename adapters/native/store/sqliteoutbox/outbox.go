package sqliteoutbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/logging"
)

// Store implements ports.OutboxStore using SQLite for local durable
// persistence in tests and single-process deployments.
//
// All SDK interaction (database/sql, modernc.org/sqlite) is confined
// to the acl_*.go files in this package; this file is the port-side
// implementation and intentionally imports no driver packages.
type Store struct {
	sess   *sqlSession
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
	sess, err := openSession(dbPath)
	if err != nil {
		return nil, err
	}

	s := &Store{sess: sess, clk: clock.System}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.sess.close()
}

// Persist inserts the supplied records in a single transaction.
func (s *Store) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: persist", "count", len(records))
	}
	return s.sess.persist(ctx, records, s.clk)
}

// Claim atomically claims up to limit pending records under partition pk.
func (s *Store) Claim(ctx context.Context, pk string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: claim", "partition_key", pk, "limit", limit)
	}
	return s.sess.claim(ctx, pk, token, limit)
}

// Complete marks the supplied records as completed at the current clock time.
func (s *Store) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: complete", "count", len(recordIDs))
	}
	return s.sess.complete(ctx, recordIDs, token, s.clk.Now())
}

// Expire marks pending records whose expires_at is older than before
// as expired. Claimed records are never expired here.
func (s *Store) Expire(ctx context.Context, before time.Time) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: expire")
	}
	return s.sess.expire(ctx, before)
}

// QueryPending returns up to limit pending records under partition pk.
func (s *Store) QueryPending(ctx context.Context, pk string, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: query_pending", "partition_key", pk, "limit", limit)
	}
	return s.sess.queryPending(ctx, pk, limit)
}

// partitionKey derives the storage partition for a record.
//
// Lives in the port-side file because it operates purely on domain
// types — no SQL, no SDK.
func partitionKey(r *persistence.OutboxRecord) string {
	if r.SessionID() != "" {
		return "SESSION#" + r.SessionID()
	}
	return "BINDING#" + r.BindingID()
}

package sqlitedlq

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// Compile-time interface assertion. io.Closer lets a lifecycle-aware
// composition root release the file handle on stop/reload without importing
// this package's concrete type. ports.DLQStore is satisfied structurally
// and asserted in the test package to keep this production file free of a
// ports import (see .go-arch-lint.yml).
var _ io.Closer = (*Store)(nil)

// Store implements ports.DLQStore using SQLite for local durable
// persistence in tests and single-process deployments.
//
// All SDK interaction (database/sql, modernc.org/sqlite) is confined
// to the acl_*.go files in this package; this file is the port-side
// implementation and intentionally imports no driver packages.
type Store struct {
	sess   *sqlSession
	logger *slog.Logger

	// metrics receives the store-health counter (MetricStoreUnhealthy) on a
	// fatal storage fault. Defaults to a no-op meter so the store never depends
	// on a configured backend; injectable via WithMetrics. Mirrors sqliteoutbox.
	metrics counterMeter

	// retention, when > 0, opts this store into a piggybacked retention sweep
	// that purges entries older than the window on Write, bounding disk growth.
	// clk is the sweep's clock (clock.System by default, injectable via
	// WithClock for deterministic tests), mirroring sqliteoutbox.
	// sweepMu guards the throttle bookkeeping (lastSweep) only.
	retention time.Duration
	clk       clock.Clock
	sweepMu   sync.Mutex
	lastSweep time.Time
}

// Option configures a Store at construction.
type Option func(*Store)

// WithRetention opts the store into a best-effort retention sweep: entries
// whose failed_at precedes now-d are purged opportunistically on Write
// (throttled), bounding otherwise-unbounded DLQ disk growth. A value <= 0
// (the default) disables the sweep entirely — the historical behaviour.
// Mirrors sqliteoutbox's retention and dynamodbdlq's TTL.
func WithRetention(d time.Duration) Option {
	return func(s *Store) { s.retention = d }
}

// WithClock overrides the clock used by the retention sweep.
// Defaults to clock.System when nil or not set.
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithLogger sets a structured logger for trace-level diagnostics and
// retention-sweep failures.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// WithMetrics sets the metrics exporter the store emits store-level counters
// through (MetricStoreUnhealthy on a fatal storage fault). A nil exporter
// leaves the no-op meter in place, so the store never depends on a configured
// backend; the parameter is the minimal counterMeter surface and
// ports.MetricsExporter satisfies it structurally, so the composition root can
// inject a real exporter with no adapter glue. Mirrors sqliteoutbox.WithMetrics.
func WithMetrics(m counterMeter) Option {
	return func(s *Store) {
		if m != nil {
			s.metrics = m
		}
	}
}

// SetLogger sets a structured logger for trace-level diagnostics.
//
// Deprecated: prefer WithLogger at construction. Retained for backward
// compatibility; it does not participate in the functional-options flow.
func (s *Store) SetLogger(l *slog.Logger) { s.logger = l }

// NewStore opens (or creates) a SQLite database at dbPath and runs the
// schema migration. Use ":memory:" for a purely in-memory database.
func NewStore(dbPath string, opts ...Option) (*Store, error) {
	sess, err := openSession(dbPath)
	if err != nil {
		return nil, err
	}
	s := &Store{sess: sess, clk: clock.System, metrics: noopMeter{}}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.sess.close()
}

// Write inserts a single DLQ entry. Duplicates surface as
// shared.ErrDuplicateRecord. When a retention window is configured (see
// WithRetention) a throttled purge of expired entries is triggered afterward.
func (s *Store) Write(ctx context.Context, entry routing.DLQEntry) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: write",
			"route_id", entry.RouteID(), "entry_id", entry.ID())
	}
	err := s.sess.write(ctx, entry)
	s.maybeSweep(ctx)
	return s.observe(ctx, err)
}

// observe emits the store-health counter (MetricStoreUnhealthy) when err is a
// fatal storage fault — disk full, corruption, read-only, or not-a-database —
// and returns err unchanged. mapError already classifies such faults as
// PERMANENT; surfacing them here as a metric (and an error log) gives operators
// a dashboard signal for a failing last-resort DLQ sink that no retry can
// clear, instead of it blending into transient-throttle noise. Non-fatal
// errors pass through untouched. Mirrors sqliteoutbox.Store.observe.
func (s *Store) observe(ctx context.Context, err error) error {
	if err != nil && isFatalStorageErr(err) {
		s.metrics.Counter(MetricStoreUnhealthy, 1,
			shared.Tag{Key: shared.TagKeyEntity, Value: "dlq"})
		if s.logger != nil {
			s.logger.LogAttrs(ctx, slog.LevelError, "sqlitedlq: fatal storage fault",
				slog.Any("error", err))
		}
	}
	return err
}

// maybeSweep opportunistically purges entries older than the retention window,
// throttled to at most once per max(retention/4, time.Minute) so the hot Write
// path rarely touches the disk for cleanup. Retention <= 0 disables it. Sweep
// failures are non-fatal (the Write already succeeded/failed on its own merits)
// but are surfaced at Warn so a disk-full/corruption fault is not lost.
func (s *Store) maybeSweep(ctx context.Context) {
	if s.retention <= 0 {
		return
	}
	now := s.clk.Now()

	s.sweepMu.Lock()
	interval := s.retention / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < interval {
		s.sweepMu.Unlock()
		return
	}
	s.lastSweep = now
	s.sweepMu.Unlock()

	if _, err := s.sess.purge(ctx, now.Add(-s.retention)); err != nil && s.logger != nil {
		s.logger.Warn("sqlitedlq: retention sweep failed", "error", err)
	}
}

// List returns DLQ entries matching the supplied filter, oldest first
// (ORDER BY failed_at ASC, id ASC — matches the DLQStore contract and the
// other backends).
func (s *Store) List(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: list",
			"route_id", filter.RouteID, "limit", filter.Limit)
	}
	entries, err := s.sess.list(ctx, filter)
	return entries, s.observe(ctx, err)
}

// Get returns the entry with id or shared.ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (routing.DLQEntry, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: get", "entry_id", id)
	}
	entry, err := s.sess.get(ctx, id)
	return entry, s.observe(ctx, err)
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
	n, err := s.sess.delete(ctx, ids)
	return n, s.observe(ctx, err)
}

// DeleteByFilter removes every entry matching the filter.
func (s *Store) DeleteByFilter(ctx context.Context, filter routing.DLQFilter) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: delete_by_filter",
			"route_id", filter.RouteID, "category", filter.Category)
	}
	n, err := s.sess.deleteByFilter(ctx, filter)
	return n, s.observe(ctx, err)
}

// Purge deletes every entry whose failed_at is strictly before the
// supplied cutoff. Returns the number of entries deleted.
func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: purge", "before", before)
	}
	n, err := s.sess.purge(ctx, before)
	return n, s.observe(ctx, err)
}

// Depth reports the current number of outstanding DLQ entries — the standing
// backlog behind shared.MetricDLQDepth (ports.DLQDepthReporter, sampled by
// runtime.ReportDLQDepth). Backed by an efficient COUNT(*), not a paging scan.
// A transient backend read error is returned as-is (the caller treats it as
// "depth unavailable this sample" and emits nothing).
func (s *Store) Depth(ctx context.Context) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqlitedlq: depth")
	}
	n, err := s.sess.count(ctx)
	return n, s.observe(ctx, err)
}

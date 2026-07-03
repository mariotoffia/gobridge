package sqliteoutbox

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/logging"
)

// Compile-time interface assertions. io.Closer lets a lifecycle-aware
// composition root release the file handle on stop/reload without importing
// this package's concrete type (I5). ports.OutboxStore/OutboxReleaser are
// satisfied structurally and asserted in the test package to keep this
// production file free of a ports import (see .go-arch-lint.yml).
var _ io.Closer = (*Store)(nil)

const (
	// defaultRetention mirrors the DynamoDB backend's default compaction
	// grace (WithCompactionGrace): terminal rows survive at least this long
	// before compaction may delete them.
	defaultRetention = time.Hour

	// fenceRetentionFloor bounds how aggressively fence rows may be
	// compacted: a fence is deleted only after max(retention,
	// fenceRetentionFloor) without any Claim touching its partition.
	// Losing a fence after 30 days of abandonment is acceptable — such a
	// partition has no competing owners left to fence — and it stops
	// ephemeral/rotating session partitions from accreting one immortal
	// fence row each.
	fenceRetentionFloor = 30 * 24 * time.Hour
)

// Store implements ports.OutboxStore using SQLite for local durable
// persistence in tests and single-process deployments.
//
// All SDK interaction (database/sql, modernc.org/sqlite) is confined
// to the acl_*.go files in this package; this file is the port-side
// implementation and intentionally imports no driver packages.
type Store struct {
	sess       *sqlSession
	clk        clock.Clock
	staleClaim time.Duration
	retention  time.Duration
	logger     *slog.Logger

	// lastCompactMs throttles piggybacked retention compaction (unix ms of
	// the last run). Accessed atomically; Store methods may race.
	lastCompactMs atomic.Int64
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

// WithStaleClaimDuration enables time-based reclaim of stranded claims (I1).
// When d > 0 a Claim may additionally reclaim a record that is still claimed
// but whose claim is older than d, recovering work orphaned by an owner that
// crashed between Claim and Complete/Release. When d <= 0 (the default) the
// store stays strictly version-only, matching the historical behaviour. The
// bridge derives d from ports.OutboxRuntimeOptions.StaleClaimDuration.
func WithStaleClaimDuration(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.staleClaim = d
		}
	}
}

// WithLogger sets a structured logger for trace-level diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// WithRetention sets the retention window for terminal rows: completed and
// expired records older than d are physically deleted by a best-effort
// compaction piggybacked (throttled) on Complete and Expire, so the
// single-file production backend does not grow unboundedly. Fence rows of
// partitions untouched for max(d, 30 days) are deleted too.
//
// Deleting a terminal row releases its duplicate-detection identity
// (envelope ID + binding ID), so d must comfortably exceed any upstream
// redelivery window — the same tradeoff as the DynamoDB backend's item TTL.
// Defaults to one hour (mirroring DynamoDB's compaction grace); d <= 0
// disables compaction entirely (pre-retention behaviour: unbounded growth).
func WithRetention(d time.Duration) Option {
	return func(s *Store) { s.retention = d }
}

// NewStore opens (or creates) a SQLite database at dbPath and runs the
// schema migration. Use ":memory:" for a purely in-memory database.
func NewStore(dbPath string, opts ...Option) (*Store, error) {
	// Options first: openSession stamps legacy fence rows with the
	// (possibly test-injected) clock's now.
	s := &Store{clk: clock.System, retention: defaultRetention}
	for _, o := range opts {
		o(s)
	}

	sess, err := openSession(dbPath, s.clk.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	s.sess = sess
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
	return s.sess.claim(ctx, pk, token, limit, s.clk.Now(), s.staleClaim)
}

// Complete marks the supplied records as completed at the current clock time.
// A successful Complete may piggyback a throttled retention compaction pass
// (see WithRetention).
func (s *Store) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: complete", "count", len(recordIDs))
	}
	if err := s.sess.complete(ctx, recordIDs, token, s.clk.Now()); err != nil {
		return err
	}
	s.maybeCompact(ctx)
	return nil
}

// Release returns the supplied records from claimed to pending so the
// same owner can re-claim and retry them on the next drain after a
// transient egress failure, without a fencing-version bump or a
// wall-clock stale-claim wait. Fencing is owner+version+status,
// identical to Complete.
func (s *Store) Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: release", "count", len(recordIDs))
	}
	return s.sess.release(ctx, recordIDs, token)
}

// Expire marks pending records whose expires_at is older than before
// as expired. Claimed records are never expired here. A successful
// Expire may piggyback a throttled retention compaction pass (see
// WithRetention).
func (s *Store) Expire(ctx context.Context, before time.Time) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: expire")
	}
	n, err := s.sess.expire(ctx, before)
	if err != nil {
		return 0, err
	}
	s.maybeCompact(ctx)
	return n, nil
}

// maybeCompact runs a best-effort retention compaction pass when compaction
// is enabled and the throttle interval (a quarter of the retention window,
// at least one minute) has elapsed since the last pass. Failures are logged,
// never surfaced: housekeeping must not fail the foreground operation it
// piggybacks on.
func (s *Store) maybeCompact(ctx context.Context) {
	if s.retention <= 0 {
		return
	}
	now := s.clk.Now()
	interval := s.retention / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	last := s.lastCompactMs.Load()
	if last != 0 && now.Sub(time.UnixMilli(last)) < interval {
		return
	}
	if !s.lastCompactMs.CompareAndSwap(last, now.UnixMilli()) {
		return // another goroutine won this pass
	}

	fenceRetention := s.retention
	if fenceRetention < fenceRetentionFloor {
		fenceRetention = fenceRetentionFloor
	}
	if err := s.sess.compact(ctx, now.Add(-s.retention), now.Add(-fenceRetention)); err != nil {
		if s.logger != nil {
			s.logger.LogAttrs(ctx, slog.LevelWarn, "sqliteoutbox: retention compaction failed",
				slog.Any("error", err))
		}
	}
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

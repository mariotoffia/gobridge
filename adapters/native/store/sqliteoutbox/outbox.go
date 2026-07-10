package sqliteoutbox

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	metrics    counterMeter

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

// WithMetrics sets the metrics exporter the store emits store-level counters
// through (MetricStoreUnhealthy on a fatal storage fault). A nil exporter
// leaves the no-op meter in place, so the store never depends on a configured
// backend; the parameter is the minimal counterMeter surface and
// ports.MetricsExporter satisfies it structurally, so the composition root can
// inject a real exporter with no adapter glue.
func WithMetrics(m counterMeter) Option {
	return func(s *Store) {
		if m != nil {
			s.metrics = m
		}
	}
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
	s := &Store{clk: clock.System, retention: defaultRetention, metrics: noopMeter{}}
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
	return s.observe(ctx, s.sess.persist(ctx, records, s.clk))
}

// Claim atomically claims up to limit pending records under partition pk.
func (s *Store) Claim(ctx context.Context, pk string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	// F1 fencing guard: reject a zero-value / invalid LeaseToken (empty owner
	// OR zero version, persistence.LeaseToken.Valid) at the facade BEFORE any
	// SQL runs, so the raw claim UPDATE never fences on an empty owner / zero
	// version. The inner sqlSession.claim is only reachable through this method
	// (single entry point), so this guard alone keeps the raw SQL from ever
	// executing with a bad token.
	if !token.Valid() {
		return nil, shared.ErrStaleFencingToken.
			WithMessage("claim rejected: invalid (zero-value) fencing token").
			With("givenOwner", token.Owner).
			With("givenVersion", token.Version)
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: claim", "partition_key", pk, "limit", limit)
	}
	recs, err := s.sess.claim(ctx, pk, token, limit, s.clk.Now(), s.staleClaim)
	return recs, s.observe(ctx, err)
}

// Complete marks the supplied records as completed at the current clock time.
// A successful Complete may piggyback a throttled retention compaction pass
// (see WithRetention).
func (s *Store) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	// F1 fencing guard: reject a zero-value / invalid LeaseToken at the facade
	// BEFORE any SQL runs (defense-in-depth: the guarded UPDATE's owner+version
	// fence also rejects it, but the explicit guard keeps the raw SQL from
	// running with a bad token).
	if !token.Valid() {
		return shared.ErrStaleFencingToken.
			WithMessage("complete rejected: invalid (zero-value) fencing token").
			With("givenOwner", token.Owner).
			With("givenVersion", token.Version)
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: complete", "count", len(recordIDs))
	}
	if err := s.sess.complete(ctx, recordIDs, token, s.clk.Now()); err != nil {
		return s.observe(ctx, err)
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
	// F1 fencing guard: reject a zero-value / invalid LeaseToken at the facade
	// BEFORE any SQL runs (defense-in-depth alongside the guarded UPDATE's
	// owner+version fence).
	if !token.Valid() {
		return shared.ErrStaleFencingToken.
			WithMessage("release rejected: invalid (zero-value) fencing token").
			With("givenOwner", token.Owner).
			With("givenVersion", token.Version)
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: release", "count", len(recordIDs))
	}
	return s.observe(ctx, s.sess.release(ctx, recordIDs, token))
}

// Expire marks pending records whose expires_at is older than before
// as expired, SCOPED to the supplied partition (M1). Claimed records are
// never expired here, and records in other partitions are left untouched.
// A successful Expire may piggyback a throttled retention compaction pass
// (see WithRetention).
func (s *Store) Expire(ctx context.Context, before time.Time, partition string) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: expire", "partition_key", partition)
	}
	n, err := s.sess.expire(ctx, before, partition)
	if err != nil {
		return 0, s.observe(ctx, err)
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
	recs, err := s.sess.queryPending(ctx, pk, limit)
	return recs, s.observe(ctx, err)
}

// CountPending reports the number of PENDING records for pk — the true,
// unbounded backlog behind shared.MetricOutboxDepth (ports.OutboxDepthReporter).
// It is backed by an indexed COUNT(*) (idx_outbox_partition_status), never a
// record-materialising scan like QueryPending, so it stays cheap on the
// drainer's per-cycle call. An empty pk counts across every partition; a
// concrete pk scopes the count. A real backend read failure is returned as-is
// (the drainer skips the depth emission that cycle rather than masking it); the
// store genuinely reports depth, so it never returns
// ports.ErrOutboxDepthUnsupported.
func (s *Store) CountPending(ctx context.Context, pk string) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "sqliteoutbox: count_pending", "partition_key", pk)
	}
	n, err := s.sess.countPending(ctx, pk)
	return n, s.observe(ctx, err)
}

// observe emits the store-health counter (MetricStoreUnhealthy) when err is a
// fatal storage fault — disk full, corruption, read-only, or not-a-database —
// and returns err unchanged. mapError already classifies such faults as
// PERMANENT so the drain loop stops retrying; surfacing them here as a metric
// (and an error log) gives operators a dashboard signal for a condition no
// retry can clear. Non-fatal errors pass through untouched.
func (s *Store) observe(ctx context.Context, err error) error {
	if err != nil && isFatalStorageErr(err) {
		s.metrics.Counter(MetricStoreUnhealthy, 1,
			shared.Tag{Key: shared.TagKeyEntity, Value: "outbox"})
		if s.logger != nil {
			s.logger.LogAttrs(ctx, slog.LevelError, "sqliteoutbox: fatal storage fault",
				slog.Any("error", err))
		}
	}
	return err
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

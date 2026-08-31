package runtime

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// InstrumentedLeaseStore wraps a LeaseStore and records latency and
// failure metrics via a MetricsExporter.
type InstrumentedLeaseStore struct {
	inner   ports.LeaseStore
	metrics ports.MetricsExporter
	clk     clock.Clock
}

var _ ports.LeaseStore = (*InstrumentedLeaseStore)(nil)

// NewInstrumentedLeaseStore decorates inner with metrics instrumentation.
func NewInstrumentedLeaseStore(inner ports.LeaseStore, metrics ports.MetricsExporter, clk clock.Clock) *InstrumentedLeaseStore {
	return &InstrumentedLeaseStore{inner: inner, metrics: metrics, clk: instrumentedClock(clk)}
}

func (s *InstrumentedLeaseStore) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	start := s.clk.Now()
	tok, err := s.inner.Acquire(ctx, leaseID, ownerID, ttl, endpoints)
	tags := []shared.Tag{{Key: shared.TagKeyLeaseID, Value: leaseID}}

	s.metrics.Timer(shared.MetricLeaseAcquireLatency, s.clk.Since(start), tags...)
	if err != nil {
		s.metrics.Counter(shared.MetricLeaseAcquireFailures, 1, tags...)
	}
	return tok, err
}

func (s *InstrumentedLeaseStore) Renew(ctx context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	start := s.clk.Now()
	tok, err := s.inner.Renew(ctx, leaseID, token, ttl, endpoints)
	tags := []shared.Tag{{Key: shared.TagKeyLeaseID, Value: leaseID}}
	s.metrics.Timer(shared.MetricLeaseRenewLatency, s.clk.Since(start), tags...)
	return tok, err
}

func (s *InstrumentedLeaseStore) Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error {
	return s.inner.Release(ctx, leaseID, token)
}

func (s *InstrumentedLeaseStore) Current(ctx context.Context, leaseID string) (persistence.LeaseInfo, error) {
	return s.inner.Current(ctx, leaseID)
}

// InstrumentedOutboxStore wraps an OutboxStore and records latency metrics.
type InstrumentedOutboxStore struct {
	inner   ports.OutboxStore
	metrics ports.MetricsExporter
	clk     clock.Clock
}

var (
	_ ports.OutboxStore         = (*InstrumentedOutboxStore)(nil)
	_ ports.OutboxDepthReporter = (*InstrumentedOutboxStore)(nil)
)

// NewInstrumentedOutboxStore decorates inner with metrics instrumentation.
//
// This constructor returns the bare *InstrumentedOutboxStore concrete type.
// It does NOT preserve inner's optional capabilities (ports.OutboxReleaser,
// io.Closer); composition roots MUST use
// NewInstrumentedOutboxStoreCapabilityPreserving instead.
func NewInstrumentedOutboxStore(inner ports.OutboxStore, metrics ports.MetricsExporter, clk clock.Clock) *InstrumentedOutboxStore {
	return &InstrumentedOutboxStore{inner: inner, metrics: metrics, clk: instrumentedClock(clk)}
}

// NewInstrumentedOutboxStoreCapabilityPreserving decorates inner with metrics
// instrumentation while preserving its OPTIONAL capabilities (finding 14).
//
// OutboxStore carries two optional capabilities — ports.OutboxReleaser (fast
// fencing-safe release of a transiently-failed claim) and io.Closer
// (durable-handle cleanup on Stop). The bare *InstrumentedOutboxStore statically
// masks them: the drainer's `store.(ports.OutboxReleaser)` and Stop's
// `store.(io.Closer)` assertions fail even when the backing store supports them,
// silently degrading release-fast to version/stale reclaim and leaking the
// underlying handle. To preserve the DYNAMIC capability set we select, at
// construction time, the smallest wrapper that re-exports exactly the
// capabilities inner actually has (2^n variants generated only for the two
// capabilities that exist). The base OutboxStore metrics apply in every variant
// via the embedded *InstrumentedOutboxStore.
func NewInstrumentedOutboxStoreCapabilityPreserving(inner ports.OutboxStore, metrics ports.MetricsExporter, clk clock.Clock) ports.OutboxStore {
	base := &InstrumentedOutboxStore{inner: inner, metrics: metrics, clk: instrumentedClock(clk)}
	releaser, hasReleaser := inner.(ports.OutboxReleaser)
	closer, hasCloser := inner.(io.Closer)
	switch {
	case hasReleaser && hasCloser:
		return &instrumentedOutboxReleaserCloser{InstrumentedOutboxStore: base, releaser: releaser, closer: closer}
	case hasReleaser:
		return &instrumentedOutboxReleaser{InstrumentedOutboxStore: base, releaser: releaser}
	case hasCloser:
		return &instrumentedOutboxCloser{InstrumentedOutboxStore: base, closer: closer}
	default:
		return base
	}
}

// instrumentedOutboxReleaser re-exports the optional ports.OutboxReleaser
// capability of a wrapped store that also supports it.
type instrumentedOutboxReleaser struct {
	*InstrumentedOutboxStore
	releaser ports.OutboxReleaser
}

var (
	_ ports.OutboxStore    = (*instrumentedOutboxReleaser)(nil)
	_ ports.OutboxReleaser = (*instrumentedOutboxReleaser)(nil)
)

func (s *instrumentedOutboxReleaser) Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	return s.releaser.Release(ctx, recordIDs, token)
}

// instrumentedOutboxCloser re-exports the optional io.Closer capability of a
// wrapped store that also supports it.
type instrumentedOutboxCloser struct {
	*InstrumentedOutboxStore
	closer io.Closer
}

var (
	_ ports.OutboxStore = (*instrumentedOutboxCloser)(nil)
	_ io.Closer         = (*instrumentedOutboxCloser)(nil)
)

func (s *instrumentedOutboxCloser) Close() error {
	if err := s.closer.Close(); err != nil {
		return fmt.Errorf("close instrumented outbox store: %w", err)
	}
	return nil
}

// instrumentedOutboxReleaserCloser re-exports both optional capabilities of a
// wrapped store that supports OutboxReleaser and io.Closer.
type instrumentedOutboxReleaserCloser struct {
	*InstrumentedOutboxStore
	releaser ports.OutboxReleaser
	closer   io.Closer
}

var (
	_ ports.OutboxStore    = (*instrumentedOutboxReleaserCloser)(nil)
	_ ports.OutboxReleaser = (*instrumentedOutboxReleaserCloser)(nil)
	_ io.Closer            = (*instrumentedOutboxReleaserCloser)(nil)
)

func (s *instrumentedOutboxReleaserCloser) Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	return s.releaser.Release(ctx, recordIDs, token)
}

func (s *instrumentedOutboxReleaserCloser) Close() error {
	if err := s.closer.Close(); err != nil {
		return fmt.Errorf("close instrumented outbox store: %w", err)
	}
	return nil
}

func (s *InstrumentedOutboxStore) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	start := s.clk.Now()
	err := s.inner.Persist(ctx, records)
	if len(records) > 0 {
		tags := []shared.Tag{{Key: shared.TagKeyRouteID, Value: records[0].RouteID()}}
		s.metrics.Timer(shared.MetricOutboxPersistLatency, s.clk.Since(start), tags...)
	}
	return err
}

func (s *InstrumentedOutboxStore) Claim(ctx context.Context, partitionKey string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	recs, err := s.inner.Claim(ctx, partitionKey, token, limit)
	if err == nil && len(recs) > 0 {
		for _, rec := range recs {
			if rec.ReplayCount() > 1 {
				tags := []shared.Tag{{Key: shared.TagKeySessionID, Value: rec.SessionID()}}
				s.metrics.Counter(shared.MetricOutboxClaimRecoveries, 1, tags...)
			}
		}
	}
	return recs, err
}

func (s *InstrumentedOutboxStore) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	return s.inner.Complete(ctx, recordIDs, token)
}

func (s *InstrumentedOutboxStore) Expire(ctx context.Context, before time.Time, partition string, token persistence.LeaseToken) (int, error) {
	return s.inner.Expire(ctx, before, partition, token)
}

func (s *InstrumentedOutboxStore) QueryPending(ctx context.Context, partitionKey string, limit int) ([]*persistence.OutboxRecord, error) {
	recs, err := s.inner.QueryPending(ctx, partitionKey, limit)
	if err == nil {
		tags := []shared.Tag{{Key: shared.TagKeyPartition, Value: partitionKey}}
		s.metrics.Gauge(shared.MetricOutboxDepth, float64(len(recs)), tags...)
	}
	return recs, err
}

// CountPending forwards the OPTIONAL ports.OutboxDepthReporter capability of the
// wrapped store (finding-14 family: an instrumentation wrapper must not strip
// optional interfaces). The base *InstrumentedOutboxStore ALWAYS satisfies
// ports.OutboxDepthReporter so the drainer's capability probe succeeds in
// production (where it holds the wrapper, not the raw store); when the inner
// store cannot count, CountPending returns the EXPORTED sentinel
// ports.ErrOutboxDepthUnsupported rather than a bogus number, so the drainer can
// distinguish "unsupported" (benign saturating fallback) from a REAL count
// failure (which it surfaces, never masks). Advertising the method
// unconditionally is SAFE here — unlike the Release/Started capabilities, a
// false-positive cannot panic or wedge, it only yields the sentinel the caller
// already handles by falling back. A real error from the inner reporter is
// returned AS-IS (not the sentinel) so it is NOT mistaken for "unsupported".
// This method does NOT emit a metric; the drainer owns the
// shared.MetricOutboxDepth emission so there is a single drain-path emission
// site.
func (s *InstrumentedOutboxStore) CountPending(ctx context.Context, partitionKey string) (int, error) {
	reporter, ok := s.inner.(ports.OutboxDepthReporter)
	if !ok {
		return 0, ports.ErrOutboxDepthUnsupported
	}
	return reporter.CountPending(ctx, partitionKey)
}

// CountClaimed forwards the OPTIONAL ports.OutboxClaimedDepthReporter capability
// of the wrapped store, on the same terms as CountPending: the wrapper ALWAYS
// satisfies the interface so the drainer's capability probe succeeds in
// production (where it holds the wrapper, not the raw store), and returns the
// EXPORTED sentinel ports.ErrOutboxDepthUnsupported when the inner store has not
// adopted it. A real error from the inner reporter is returned AS-IS so it is
// not mistaken for "unsupported". It emits no metric; the drainer owns the
// shared.MetricOutboxClaimedDepth emission so there is a single emission site.
func (s *InstrumentedOutboxStore) CountClaimed(ctx context.Context, partitionKey string) (int, error) {
	reporter, ok := s.inner.(ports.OutboxClaimedDepthReporter)
	if !ok {
		return 0, ports.ErrOutboxDepthUnsupported
	}
	return reporter.CountClaimed(ctx, partitionKey)
}

func instrumentedClock(clk clock.Clock) clock.Clock {
	if clk == nil {
		return clock.System
	}
	return clk
}

// ReportDLQDepth samples the current dead-letter-queue backlog via the OPTIONAL
// ports.DLQDepthReporter capability and emits it as the shared.MetricDLQDepth
// gauge. It is the store-side means of making DLQ depth observable: composition
// roots call it on a periodic cadence (and MAY call it after DLQ mutations) so a
// stale backlog is alarmable without a manual storage scan (H-OBS DLQ-1).
//
// Fail-safe by design:
//   - When store does not implement ports.DLQDepthReporter it emits nothing and
//     returns (0, false, nil) — a store without the capability is not an error.
//   - When Depth returns an error it emits nothing and returns (0, false, err).
//   - Otherwise it emits shared.MetricDLQDepth with the supplied tags (pass none
//     for the recommended dimensionless fleet total) and returns (depth, true,
//     nil).
//
// metrics may be nil (no-op emission) so a caller can use the returned depth for
// its own purposes without an exporter.
func ReportDLQDepth(ctx context.Context, store ports.DLQReader, metrics ports.MetricsExporter, tags ...shared.Tag) (int, bool, error) {
	reporter, ok := store.(ports.DLQDepthReporter)
	if !ok {
		return 0, false, nil
	}
	depth, err := reporter.Depth(ctx)
	if err != nil {
		return 0, false, err
	}
	if metrics != nil {
		metrics.Gauge(shared.MetricDLQDepth, float64(depth), tags...)
	}
	return depth, true, nil
}

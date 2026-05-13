package runtime

import (
	"context"
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

var _ ports.OutboxStore = (*InstrumentedOutboxStore)(nil)

// NewInstrumentedOutboxStore decorates inner with metrics instrumentation.
func NewInstrumentedOutboxStore(inner ports.OutboxStore, metrics ports.MetricsExporter, clk clock.Clock) *InstrumentedOutboxStore {
	return &InstrumentedOutboxStore{inner: inner, metrics: metrics, clk: instrumentedClock(clk)}
}

func (s *InstrumentedOutboxStore) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	start := s.clk.Now()
	err := s.inner.Persist(ctx, records)
	if len(records) > 0 {
		tags := []shared.Tag{{Key: shared.TagKeyRouteID, Value: records[0].RouteID}}
		s.metrics.Timer(shared.MetricOutboxPersistLatency, s.clk.Since(start), tags...)
	}
	return err
}

func (s *InstrumentedOutboxStore) Claim(ctx context.Context, partitionKey, ownerID string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	recs, err := s.inner.Claim(ctx, partitionKey, ownerID, token, limit)
	if err == nil && len(recs) > 0 {
		for _, rec := range recs {
			if rec.ReplayCount() > 1 {
				tags := []shared.Tag{{Key: shared.TagKeySessionID, Value: rec.SessionID}}
				s.metrics.Counter(shared.MetricOutboxClaimRecoveries, 1, tags...)
			}
		}
	}
	return recs, err
}

func (s *InstrumentedOutboxStore) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	return s.inner.Complete(ctx, recordIDs, token)
}

func (s *InstrumentedOutboxStore) Expire(ctx context.Context, before time.Time) (int, error) {
	return s.inner.Expire(ctx, before)
}

func (s *InstrumentedOutboxStore) QueryPending(ctx context.Context, partitionKey string, limit int) ([]*persistence.OutboxRecord, error) {
	recs, err := s.inner.QueryPending(ctx, partitionKey, limit)
	if err == nil {
		tags := []shared.Tag{{Key: shared.TagKeyPartition, Value: partitionKey}}
		s.metrics.Gauge(shared.MetricOutboxDepth, float64(len(recs)), tags...)
	}
	return recs, err
}

func instrumentedClock(clk clock.Clock) clock.Clock {
	if clk == nil {
		return clock.System
	}
	return clk
}

package runtime

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// InstrumentedLeaseStore wraps a LeaseStore and records latency and
// failure metrics via a MetricsExporter.
type InstrumentedLeaseStore struct {
	inner   ports.LeaseStore
	metrics ports.MetricsExporter
}

var _ ports.LeaseStore = (*InstrumentedLeaseStore)(nil)

// NewInstrumentedLeaseStore decorates inner with metrics instrumentation.
func NewInstrumentedLeaseStore(inner ports.LeaseStore, metrics ports.MetricsExporter) *InstrumentedLeaseStore {
	return &InstrumentedLeaseStore{inner: inner, metrics: metrics}
}

func (s *InstrumentedLeaseStore) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration) (domain.LeaseToken, error) {
	start := time.Now()
	tok, err := s.inner.Acquire(ctx, leaseID, ownerID, ttl)
	tags := []domain.Tag{{Key: domain.TagKeyLeaseID, Value: leaseID}}

	s.metrics.Timer(domain.MetricLeaseAcquireLatency, time.Since(start), tags...)
	if err != nil {
		s.metrics.Counter(domain.MetricLeaseAcquireFailures, 1, tags...)
	}
	return tok, err
}

func (s *InstrumentedLeaseStore) Renew(ctx context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration) (domain.LeaseToken, error) {
	start := time.Now()
	tok, err := s.inner.Renew(ctx, leaseID, token, ttl)
	tags := []domain.Tag{{Key: domain.TagKeyLeaseID, Value: leaseID}}
	s.metrics.Timer(domain.MetricLeaseRenewLatency, time.Since(start), tags...)
	return tok, err
}

func (s *InstrumentedLeaseStore) Release(ctx context.Context, leaseID string, token domain.LeaseToken) error {
	return s.inner.Release(ctx, leaseID, token)
}

func (s *InstrumentedLeaseStore) Current(ctx context.Context, leaseID string) (domain.LeaseInfo, error) {
	return s.inner.Current(ctx, leaseID)
}

// InstrumentedOutboxStore wraps an OutboxStore and records latency metrics.
type InstrumentedOutboxStore struct {
	inner   ports.OutboxStore
	metrics ports.MetricsExporter
}

var _ ports.OutboxStore = (*InstrumentedOutboxStore)(nil)

// NewInstrumentedOutboxStore decorates inner with metrics instrumentation.
func NewInstrumentedOutboxStore(inner ports.OutboxStore, metrics ports.MetricsExporter) *InstrumentedOutboxStore {
	return &InstrumentedOutboxStore{inner: inner, metrics: metrics}
}

func (s *InstrumentedOutboxStore) Persist(ctx context.Context, records []domain.OutboxRecord) error {
	start := time.Now()
	err := s.inner.Persist(ctx, records)
	if len(records) > 0 {
		tags := []domain.Tag{{Key: domain.TagKeyRouteID, Value: records[0].RouteID}}
		s.metrics.Timer(domain.MetricOutboxPersistLatency, time.Since(start), tags...)
	}
	return err
}

func (s *InstrumentedOutboxStore) Claim(ctx context.Context, partitionKey, ownerID string, token domain.LeaseToken, limit int) ([]domain.OutboxRecord, error) {
	recs, err := s.inner.Claim(ctx, partitionKey, ownerID, token, limit)
	if err == nil && len(recs) > 0 {
		for _, rec := range recs {
			if rec.ReplayCount > 1 {
				tags := []domain.Tag{{Key: domain.TagKeySessionID, Value: rec.SessionID}}
				s.metrics.Counter(domain.MetricOutboxClaimRecoveries, 1, tags...)
			}
		}
	}
	return recs, err
}

func (s *InstrumentedOutboxStore) Complete(ctx context.Context, recordIDs []string, token domain.LeaseToken) error {
	return s.inner.Complete(ctx, recordIDs, token)
}

func (s *InstrumentedOutboxStore) Expire(ctx context.Context, before time.Time) (int, error) {
	return s.inner.Expire(ctx, before)
}

func (s *InstrumentedOutboxStore) QueryPending(ctx context.Context, partitionKey string, limit int) ([]domain.OutboxRecord, error) {
	recs, err := s.inner.QueryPending(ctx, partitionKey, limit)
	if err == nil {
		tags := []domain.Tag{{Key: domain.TagKeyPartition, Value: partitionKey}}
		s.metrics.Gauge(domain.MetricOutboxDepth, float64(len(recs)), tags...)
	}
	return recs, err
}

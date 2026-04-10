package memoryoutbox

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/domain"
)

// Store implements ports.OutboxStore in memory for unit tests.
// It is not safe for production deployments.
type Store struct {
	mu      sync.Mutex
	records map[string]*domain.OutboxRecord // keyed by record ID
	dedup   map[string]bool                 // keyed by "EnvelopeID\x00BindingID"
	now     func() time.Time
	logger  *slog.Logger
}

// Option configures a Store.
type Option func(*Store)

// WithClock overrides the time source (defaults to time.Now).
func WithClock(fn func() time.Time) Option {
	return func(s *Store) { s.now = fn }
}

// WithLogger sets a structured logger for trace-level diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// NewStore creates a new in-memory OutboxStore.
func NewStore(opts ...Option) *Store {
	s := &Store{
		records: make(map[string]*domain.OutboxRecord),
		dedup:   make(map[string]bool),
		now:     time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func dedupKey(envelopeID, bindingID string) string {
	return envelopeID + "\x00" + bindingID
}

func partitionKey(r *domain.OutboxRecord) string {
	return domain.OutboxPartitionKey(r.SessionID, r.BindingID)
}

func (s *Store) Persist(ctx context.Context, records []domain.OutboxRecord) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryoutbox: persist",
			"count", len(records))
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range records {
		dk := dedupKey(records[i].EnvelopeID, records[i].BindingID)
		if s.dedup[dk] {
			return domain.ErrDuplicateRecord.
				WithMessage("duplicate outbox record").
				With("envelopeID", records[i].EnvelopeID).
				With("bindingID", records[i].BindingID)
		}
	}

	now := s.now()
	for i := range records {
		r := records[i] // copy
		r.Status = domain.OutboxPending
		if r.CreatedAt.IsZero() {
			r.CreatedAt = now
		}
		s.records[r.ID] = &r
		s.dedup[dedupKey(r.EnvelopeID, r.BindingID)] = true
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, pk string, ownerID string, token domain.LeaseToken, limit int) ([]domain.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryoutbox: claim",
			"partition_key", pk, "owner_id", ownerID, "limit", limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var candidates []*domain.OutboxRecord
	for _, r := range s.records {
		if partitionKey(r) != pk {
			continue
		}
		switch {
		case r.Status == domain.OutboxPending:
			candidates = append(candidates, r)
		case r.Status == domain.OutboxClaimed && r.ClaimVersion < token.Version:
			candidates = append(candidates, r)
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})

	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}

	now := s.now()
	result := make([]domain.OutboxRecord, 0, len(candidates))
	for _, r := range candidates {
		r.Status = domain.OutboxClaimed
		r.ClaimedBy = ownerID
		r.ClaimedAt = now
		r.ClaimVersion = token.Version
		r.ReplayCount++
		result = append(result, *r)
	}

	return result, nil
}

func (s *Store) Complete(ctx context.Context, recordIDs []string, token domain.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryoutbox: complete",
			"count", len(recordIDs))
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range recordIDs {
		r, ok := s.records[id]
		if !ok {
			continue
		}
		if r.ClaimVersion != token.Version {
			return domain.ErrStaleFencingToken.
				WithMessage("claim version mismatch on complete").
				With("recordID", id).
				With("storedClaimVersion", r.ClaimVersion).
				With("givenVersion", token.Version)
		}
		r.Status = domain.OutboxCompleted
		r.CompletedAt = s.now()
	}
	return nil
}

func (s *Store) Expire(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, r := range s.records {
		if r.ExpiresAt.IsZero() || !r.ExpiresAt.Before(before) {
			continue
		}
		if r.Status == domain.OutboxPending || r.Status == domain.OutboxClaimed {
			r.Status = domain.OutboxExpired
			count++
		}
	}
	return count, nil
}

func (s *Store) QueryPending(ctx context.Context, pk string, limit int) ([]domain.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryoutbox: query_pending",
			"partition_key", pk, "limit", limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []domain.OutboxRecord
	for _, r := range s.records {
		if partitionKey(r) != pk || r.Status != domain.OutboxPending {
			continue
		}
		result = append(result, *r)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})

	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

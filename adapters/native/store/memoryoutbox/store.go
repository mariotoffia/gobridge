package memoryoutbox

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// Store implements ports.OutboxStore in memory for unit tests.
// It is not safe for production deployments.
//
// In-memory aggregates are stored by pointer; lifecycle transitions go
// through the OutboxRecord state-machine methods so the store performs
// no direct field mutation. Reads return rehydrated aggregate copies
// so callers cannot reach back into store-owned state.
type Store struct {
	mu            sync.Mutex
	records       map[string]*persistence.OutboxRecord // keyed by record ID
	dedup         map[string]bool                      // keyed by "EnvelopeID\x00BindingID"
	clk           clock.Clock
	logger        *slog.Logger
	latestVersion map[string]uint64 // per-partition fencing token version
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

// NewStore creates a new in-memory OutboxStore.
func NewStore(opts ...Option) *Store {
	s := &Store{
		records:       make(map[string]*persistence.OutboxRecord),
		dedup:         make(map[string]bool),
		clk:           clock.System,
		latestVersion: make(map[string]uint64),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func dedupKey(envelopeID, bindingID string) string {
	return envelopeID + "\x00" + bindingID
}

func partitionKey(r *persistence.OutboxRecord) string {
	return persistence.OutboxPartitionKey(r.SessionID(), r.BindingID())
}

func cloneAggregate(r *persistence.OutboxRecord) *persistence.OutboxRecord {
	return persistence.RehydrateFromSnapshot(r.PersistenceSnapshot())
}

func (s *Store) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryoutbox: persist",
			"count", len(records))
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rec := range records {
		dk := dedupKey(rec.EnvelopeID(), rec.BindingID())
		if s.dedup[dk] {
			return shared.ErrDuplicateRecord.
				WithMessage("duplicate outbox record").
				With("envelopeID", rec.EnvelopeID()).
				With("bindingID", rec.BindingID())
		}
	}

	now := s.clk.Now()
	for _, rec := range records {
		// Stamp a default CreatedAt on the persistence DTO (the aggregate is
		// immutable) and rehydrate, so the caller cannot mutate stored state.
		snap := rec.PersistenceSnapshot()
		if snap.CreatedAt.IsZero() {
			snap.CreatedAt = now
		}
		stored := persistence.RehydrateFromSnapshot(snap)
		s.records[stored.ID()] = stored
		s.dedup[dedupKey(stored.EnvelopeID(), stored.BindingID())] = true
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, pk string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryoutbox: claim",
			"partition_key", pk, "owner_id", token.Owner, "limit", limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if token.Version < s.latestVersion[pk] {
		return nil, shared.ErrStaleFencingToken.
			WithMessage("claim rejected: token version is stale").
			With("givenVersion", token.Version).
			With("latestVersion", s.latestVersion[pk])
	}
	s.latestVersion[pk] = token.Version

	var candidates []*persistence.OutboxRecord
	for _, r := range s.records {
		if partitionKey(r) != pk {
			continue
		}
		if r.IsClaimable(token.Version) {
			candidates = append(candidates, r)
		}
	}

	// Sort by persisted created_at, then by envelopeID as a stable
	// tiebreaker so equal-millisecond timestamps are deterministic.
	sort.Slice(candidates, func(i, j int) bool {
		ci, cj := candidates[i].CreatedAt(), candidates[j].CreatedAt()
		if ci.Equal(cj) {
			return candidates[i].EnvelopeID() < candidates[j].EnvelopeID()
		}
		return ci.Before(cj)
	})

	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}

	now := s.clk.Now()
	result := make([]*persistence.OutboxRecord, 0, len(candidates))
	for _, r := range candidates {
		if claimErr := r.Claim(now, token.Owner, token.Version); claimErr != nil {
			// Should be unreachable given the IsClaimable filter; skip
			// any concurrent transition rather than failing the batch.
			continue
		}
		result = append(result, cloneAggregate(r))
	}

	return result, nil
}

func (s *Store) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
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
		if r.Status() != persistence.OutboxClaimed ||
			r.ClaimedBy() != token.Owner ||
			r.ClaimVersion() != token.Version {
			return shared.ErrStaleFencingToken.
				WithMessage("completion fence mismatch: record must be claimed by token owner at token version").
				With("recordID", id).
				With("status", string(r.Status())).
				With("storedOwner", r.ClaimedBy()).
				With("storedClaimVersion", r.ClaimVersion()).
				With("givenOwner", token.Owner).
				With("givenVersion", token.Version)
		}
		if completeErr := r.Complete(s.clk.Now()); completeErr != nil {
			return completeErr
		}
	}
	return nil
}

// Release returns the supplied records from claimed to pending so the
// same owner can re-claim and retry them on the next drain after a
// transient egress failure, without a fencing-version bump or wall-clock
// stale-claim wait. Fencing is owner+version+status, identical to
// Complete: a record is released only when it is currently claimed by
// token.Owner at token.Version. Any mismatch (missing, not claimed,
// wrong owner, wrong version) yields shared.ErrStaleFencingToken.
func (s *Store) Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryoutbox: release",
			"count", len(recordIDs))
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range recordIDs {
		r, ok := s.records[id]
		if !ok || r.Status() != persistence.OutboxClaimed ||
			r.ClaimedBy() != token.Owner ||
			r.ClaimVersion() != token.Version {
			return shared.ErrStaleFencingToken.
				WithMessage("release fence mismatch: record must be claimed by token owner at token version").
				With("recordID", id).
				With("status", statusOf(r)).
				With("storedOwner", ownerOf(r)).
				With("storedClaimVersion", claimVersionOf(r)).
				With("givenOwner", token.Owner).
				With("givenVersion", token.Version)
		}
		if releaseErr := r.Release(s.clk.Now()); releaseErr != nil {
			return releaseErr
		}
	}
	return nil
}

// statusOf, ownerOf and claimVersionOf safely read fence metadata from a
// possibly-nil record so the Release mismatch error is informative even
// when the record id is unknown.
func statusOf(r *persistence.OutboxRecord) string {
	if r == nil {
		return "missing"
	}
	return string(r.Status())
}

func ownerOf(r *persistence.OutboxRecord) string {
	if r == nil {
		return ""
	}
	return r.ClaimedBy()
}

func claimVersionOf(r *persistence.OutboxRecord) uint64 {
	if r == nil {
		return 0
	}
	return r.ClaimVersion()
}

func (s *Store) Expire(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	now := s.clk.Now()
	for _, r := range s.records {
		if r.Status() != persistence.OutboxPending {
			continue
		}
		if r.ExpiresAt().IsZero() || !r.ExpiresAt().Before(before) {
			continue
		}
		if expireErr := r.Expire(now); expireErr == nil {
			count++
		}
	}
	return count, nil
}

func (s *Store) QueryPending(ctx context.Context, pk string, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryoutbox: query_pending",
			"partition_key", pk, "limit", limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []*persistence.OutboxRecord
	for _, r := range s.records {
		if partitionKey(r) != pk || r.Status() != persistence.OutboxPending {
			continue
		}
		result = append(result, cloneAggregate(r))
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt().Before(result[j].CreatedAt())
	})

	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

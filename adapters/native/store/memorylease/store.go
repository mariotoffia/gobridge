package memorylease

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
)

type leaseEntry struct {
	owner     string
	version   uint64
	expiresAt time.Time
	endpoints map[string]string
}

// Store implements ports.LeaseStore in memory for tests and
// single-process mode. It is not safe for clustered production deployments.
type Store struct {
	mu      sync.Mutex
	leases  map[string]*leaseEntry
	nextVer atomic.Uint64
	now     func() time.Time // injectable clock for testing
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

// NewStore creates a new in-memory LeaseStore.
func NewStore(opts ...Option) *Store {
	s := &Store{
		leases: make(map[string]*leaseEntry),
		now:    time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Store) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memorylease: acquire",
			"lease_id", leaseID, "owner_id", ownerID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if e, ok := s.leases[leaseID]; ok && now.Before(e.expiresAt) {
		return domain.LeaseToken{}, domain.ErrAlreadyExists.
			WithMessage("lease already held").
			With("leaseID", leaseID).
			With("owner", e.owner)
	}

	ver := s.nextVer.Add(1)
	s.leases[leaseID] = &leaseEntry{
		owner:     ownerID,
		version:   ver,
		expiresAt: now.Add(ttl),
		endpoints: endpoints,
	}

	return domain.LeaseToken{Version: ver, Owner: ownerID}, nil
}

func (s *Store) Renew(ctx context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memorylease: renew",
			"lease_id", leaseID, "owner_id", token.Owner)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.leases[leaseID]
	if !ok {
		return domain.LeaseToken{}, domain.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}

	now := s.now()
	if !now.Before(e.expiresAt) {
		return domain.LeaseToken{}, domain.ErrStaleFencingToken.
			WithMessage("lease expired, must re-acquire").
			With("leaseID", leaseID).
			With("expiredAt", e.expiresAt)
	}

	if e.version != token.Version || e.owner != token.Owner {
		return domain.LeaseToken{}, domain.ErrStaleFencingToken.
			WithMessage("lease token mismatch on renew").
			With("leaseID", leaseID).
			With("storedVersion", e.version).
			With("givenVersion", token.Version)
	}

	e.expiresAt = now.Add(ttl)
	if endpoints != nil {
		e.endpoints = endpoints
	}
	return token, nil
}

func (s *Store) Release(ctx context.Context, leaseID string, token domain.LeaseToken) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memorylease: release",
			"lease_id", leaseID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.leases[leaseID]
	if !ok {
		return domain.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}

	if e.version != token.Version || e.owner != token.Owner {
		return domain.ErrStaleFencingToken.
			WithMessage("lease token mismatch on release").
			With("leaseID", leaseID).
			With("storedVersion", e.version).
			With("givenVersion", token.Version)
	}

	delete(s.leases, leaseID)
	return nil
}

func (s *Store) Current(_ context.Context, leaseID string) (domain.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.leases[leaseID]
	if !ok {
		return domain.LeaseInfo{}, domain.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}

	return domain.LeaseInfo{
		LeaseID:   leaseID,
		Owner:     e.owner,
		Version:   e.version,
		ExpiresAt: e.expiresAt,
		Endpoints: e.endpoints,
	}, nil
}

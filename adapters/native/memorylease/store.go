package memorylease

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
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
//
// Split-brain safety gate: the store keeps a
// PER-PROCESS lease map and version counter. Two replicas of the same
// standalone deployment (e.g. a Kubernetes Deployment with replicas > 1) each
// hold an independent Store and both believe they own "the" lease, producing
// split-brain delivery (duplicate / double ownership). A pure in-memory store
// genuinely cannot coordinate across processes, so the store FAILS CLOSED:
// every operation is rejected until the operator explicitly acknowledges the
// single-replica constraint via WithAcknowledgeSingleReplica(true). That opt-in
// also emits one LOUD warning documenting the split-brain risk.
type Store struct {
	mu      sync.Mutex
	leases  map[string]*leaseEntry
	nextVer atomic.Uint64
	clk     clock.Clock // injectable clock for testing
	logger  *slog.Logger

	// ackSingleReplica records the operator's explicit assertion that this
	// process is the SOLE replica using this in-memory lease store. It gates
	// every operation (fail-closed) so a silently scaled-out deployment cannot
	// cause undetectable split-brain ownership.
	ackSingleReplica bool
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

// WithAcknowledgeSingleReplica records the operator's explicit acknowledgement
// that this process is the ONLY replica that will ever use this in-memory lease
// store. It maps to the `acknowledge_single_replica` configuration key.
//
// It exists because an in-memory lease store keeps a per-process lease map and
// cannot coordinate ownership across processes. A standalone deployment silently scaled to replicas > 1 would let
// every pod believe it owns the lease → split-brain delivery (duplicate /
// double ownership). To make that impossible to do by accident, the store
// FAILS CLOSED: every operation is rejected with shared.ErrInvalidConfig until
// this option is passed with true. Passing true asserts single-replica
// ownership and emits one LOUD warning documenting the split-brain risk.
//
// Single-node dev and tests opt in with this one line:
//
//	memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))
func WithAcknowledgeSingleReplica(ack bool) Option {
	return func(s *Store) { s.ackSingleReplica = ack }
}

// NewStore creates a new in-memory LeaseStore.
//
// Without WithAcknowledgeSingleReplica(true) the returned Store is in a
// guarded, fail-closed state: construction succeeds (the constructor cannot
// change its signature) but every operation returns shared.ErrInvalidConfig.
// See WithAcknowledgeSingleReplica.
func NewStore(opts ...Option) *Store {
	s := &Store{
		leases: make(map[string]*leaseEntry),
		clk:    clock.System,
	}
	for _, o := range opts {
		o(s)
	}
	if s.ackSingleReplica {
		s.warnSplitBrainRisk(context.Background())
	}
	return s
}

// warnSplitBrainRisk emits the mandatory LOUD warning that this in-memory lease
// store is operating in single-replica mode. It is called exactly once, at
// construction, when the operator has acknowledged the constraint. Falls back
// to slog.Default() so the warning is never silently dropped when no logger is
// injected.
func (s *Store) warnSplitBrainRisk(ctx context.Context) {
	l := s.logger
	if l == nil {
		l = slog.Default()
	}
	l.LogAttrs(ctx, slog.LevelWarn,
		"memorylease: SINGLE-REPLICA MODE acknowledged — split-brain risk",
		slog.String("risk", "split-brain"),
		slog.Bool("acknowledge_single_replica", true),
		slog.String("warning",
			"in-memory lease ownership is per-process and CANNOT coordinate across replicas; "+
				"running more than one replica (e.g. Kubernetes replicas > 1) causes duplicate / "+
				"double lease ownership — keep this deployment at exactly one replica"),
	)
}

// errNotAcknowledged is the fail-closed error returned by every operation until
// the operator asserts single-replica ownership via
// WithAcknowledgeSingleReplica(true).
func (s *Store) errNotAcknowledged() error {
	return shared.ErrInvalidConfig.
		WithMessage("memorylease: refusing to operate without single-replica acknowledgement — "+
			"an in-memory lease store cannot coordinate lease ownership across processes, so scaling "+
			"beyond one replica (e.g. Kubernetes replicas > 1) causes split-brain delivery "+
			"(duplicate / double ownership); construct with memorylease.WithAcknowledgeSingleReplica(true) "+
			"to assert this process is the sole replica").
		With("risk", "split-brain")
}

// cloneEndpoints returns an independent copy of the endpoints map so that
// neither the store's internal lease state nor a returned LeaseInfo can
// alias a caller-supplied or caller-held map (LeaseInfo is a
// read-only snapshot DTO; the LeaseStore owns the lease invariants and
// defensively copies endpoints at every boundary). Returns nil for a nil
// input to preserve the "no endpoints" signal.
func cloneEndpoints(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func (s *Store) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	// Fail-FAST for the single-replica misconfig is owned by the composition
	// root: the adapters/native/store factory rejects an un-acknowledged
	// standalone memory-lease at BUILD time (startup abort naming the
	// acknowledge_single_replica key). This per-op ErrInvalidConfig is only a
	// defense-in-depth BACKSTOP for direct NewStore callers (tests/embedders)
	// that bypass the factory. Note it is NOT a clean abort in the running
	// system: the runtime acquire loop treats every non-ErrAlreadyExists error
	// as a transient store outage and retries unbounded, so relying on this
	// per-op error alone yields fail-forever — hence the factory gate is the
	// primary control. Same rationale applies to Renew/Release/Current below.
	if !s.ackSingleReplica {
		return persistence.LeaseToken{}, s.errNotAcknowledged()
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memorylease: acquire",
			"lease_id", leaseID, "owner_id", ownerID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clk.Now()
	if e, ok := s.leases[leaseID]; ok && now.Before(e.expiresAt) {
		return persistence.LeaseToken{}, shared.ErrAlreadyExists.
			WithMessage("lease already held").
			With("leaseID", leaseID).
			With("owner", e.owner)
	}

	ver := s.nextVer.Add(1)
	s.leases[leaseID] = &leaseEntry{
		owner:     ownerID,
		version:   ver,
		expiresAt: now.Add(ttl),
		endpoints: cloneEndpoints(endpoints),
	}

	return persistence.LeaseToken{Version: ver, Owner: ownerID}, nil
}

func (s *Store) Renew(ctx context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	if !s.ackSingleReplica {
		return persistence.LeaseToken{}, s.errNotAcknowledged()
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memorylease: renew",
			"lease_id", leaseID, "owner_id", token.Owner)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.leases[leaseID]
	if !ok {
		return persistence.LeaseToken{}, shared.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}

	now := s.clk.Now()
	if !now.Before(e.expiresAt) {
		return persistence.LeaseToken{}, shared.ErrStaleFencingToken.
			WithMessage("lease expired, must re-acquire").
			With("leaseID", leaseID).
			With("expiredAt", e.expiresAt)
	}

	if e.version != token.Version || e.owner != token.Owner {
		return persistence.LeaseToken{}, shared.ErrStaleFencingToken.
			WithMessage("lease token mismatch on renew").
			With("leaseID", leaseID).
			With("storedVersion", e.version).
			With("givenVersion", token.Version)
	}

	e.expiresAt = now.Add(ttl)
	if endpoints != nil {
		e.endpoints = cloneEndpoints(endpoints)
	}
	return token, nil
}

func (s *Store) Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error {
	if !s.ackSingleReplica {
		return s.errNotAcknowledged()
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memorylease: release",
			"lease_id", leaseID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.leases[leaseID]
	if !ok {
		return shared.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}

	if e.version != token.Version || e.owner != token.Owner {
		return shared.ErrStaleFencingToken.
			WithMessage("lease token mismatch on release").
			With("leaseID", leaseID).
			With("storedVersion", e.version).
			With("givenVersion", token.Version)
	}

	delete(s.leases, leaseID)
	return nil
}

func (s *Store) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	if !s.ackSingleReplica {
		return persistence.LeaseInfo{}, s.errNotAcknowledged()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.leases[leaseID]
	if !ok {
		return persistence.LeaseInfo{}, shared.ErrNotFound.
			WithMessage("lease not found").
			With("leaseID", leaseID)
	}

	return persistence.LeaseInfo{
		LeaseID:   leaseID,
		Owner:     e.owner,
		Version:   e.version,
		ExpiresAt: e.expiresAt,
		Endpoints: cloneEndpoints(e.endpoints),
	}, nil
}

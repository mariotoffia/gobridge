package memoryrollout

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// Store is an in-memory, single-point ports.ClusterRolloutStore. It holds one
// rollout at a time under a mutex and applies the domain rollout state machine
// (persistence.Rollout) as its compare-and-set: the mutex serializes access so
// the domain's invariant checks run atomically against the current state. A new
// generation is opened only once the previous rollout is terminal (invariant
// generations are strictly monotonic.
type Store struct {
	mu        sync.Mutex
	current   *persistence.Rollout                // nil until the first Propose
	committed *persistence.CommittedRolloutConfig // nil until the first Put/seed
	clk       clock.Clock
	logger    *slog.Logger
}

// Option configures a Store.
type Option func(*Store)

// WithClock overrides the clock used to stamp rollout deadlines and ack times.
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

// NewStore creates a new in-memory ClusterRolloutStore.
func NewStore(opts ...Option) *Store {
	s := &Store{clk: clock.System}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Propose opens a new rollout at the next monotonic generation. It fails with
// ErrAlreadyExists if a rollout is currently active.
func (s *Store) Propose(ctx context.Context, proposal persistence.RolloutProposal) (persistence.Rollout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trace(ctx, "propose", 0)

	if s.current != nil && !s.current.IsTerminal() {
		return persistence.Rollout{}, shared.ErrAlreadyExists.
			WithMessage("a rollout is already active").
			With("generation", s.current.Generation()).
			With("state", string(s.current.State()))
	}

	gen := uint64(1)
	if s.current != nil {
		gen = s.current.Generation() + 1
	}
	r, err := persistence.NewRollout(gen, proposal, s.clk.Now().Add(proposal.TTL))
	if err != nil {
		return persistence.Rollout{}, err
	}
	s.current = &r
	return r, nil
}

// Ack records a member's acknowledgement, advancing Proposed -> Staging on the
// first ack.
func (s *Store) Ack(ctx context.Context, generation uint64, memberID, buildDigest string) error {
	return s.mutate(ctx, generation, "ack", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithAck(memberID, buildDigest, s.clk.Now())
	})
}

// Nack records a member's rejection without terminating the rollout.
func (s *Store) Nack(ctx context.Context, generation uint64, memberID, reason string) error {
	return s.mutate(ctx, generation, "nack", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithNack(memberID, reason)
	})
}

// Commit commits the rollout under the coordinator's fencing token. When the
// rollout carries a confirm window (design §8.1) the commit is PROVISIONAL and
// the store stamps the confirm deadline from its clock + the frozen window.
func (s *Store) Commit(ctx context.Context, generation uint64, token persistence.LeaseToken) error {
	return s.mutate(ctx, generation, "commit", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		if r.ConfirmWindow() > 0 {
			return r.WithProvisionalCommit(token, s.clk.Now().Add(r.ConfirmWindow()))
		}
		return r.WithCommit(token)
	})
}

// Converge records a member's post-swap convergence on a provisionally-committed
// generation (confirm window, design §8.1).
func (s *Store) Converge(ctx context.Context, generation uint64, memberID string) error {
	return s.mutate(ctx, generation, "converge", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithConverged(memberID, s.clk.Now())
	})
}

// Confirm confirms a provisionally-committed rollout under the coordinator's
// fencing token (confirm window success, design §8.1).
func (s *Store) Confirm(ctx context.Context, generation uint64, token persistence.LeaseToken) error {
	return s.mutate(ctx, generation, "confirm", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithConfirm(token)
	})
}

// Revert reverts a provisionally-committed rollout under the coordinator's
// fencing token (confirm window deadman, design §8.1).
func (s *Store) Revert(ctx context.Context, generation uint64, token persistence.LeaseToken, reason string) error {
	return s.mutate(ctx, generation, "revert", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithRevert(token, reason)
	})
}

// Abort aborts the rollout under the coordinator's fencing token.
func (s *Store) Abort(ctx context.Context, generation uint64, token persistence.LeaseToken, reason string) error {
	return s.mutate(ctx, generation, "abort", func(r persistence.Rollout) (persistence.Rollout, *shared.BridgeError) {
		return r.WithAbort(token, reason)
	})
}

// Current returns the current (active or last-decided) rollout, or ErrNotFound
// if none has ever been proposed.
func (s *Store) Current(ctx context.Context) (persistence.Rollout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trace(ctx, "current", 0)

	if s.current == nil {
		return persistence.Rollout{}, shared.ErrNotFound.WithMessage("no rollout has been proposed")
	}
	return *s.current, nil
}

// PutCommittedConfig durably records the last-committed config artifact under
// the store mutex, enforcing monotonicity and same-generation idempotence (see
// the port contract). A lower generation is a no-op; a same-generation Put with
// a different digest is corruption.
func (s *Store) PutCommittedConfig(ctx context.Context, cfg persistence.CommittedRolloutConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trace(ctx, "put-committed", cfg.Generation)

	if s.committed != nil {
		switch {
		case cfg.Generation < s.committed.Generation:
			return nil // stale writer: never regress the artifact
		case cfg.Generation == s.committed.Generation:
			if cfg.Digest != s.committed.Digest {
				return shared.ErrRolloutDigestMismatch.
					WithMessage("committed config digest conflict at the same generation").
					With("generation", cfg.Generation).
					With("stored", s.committed.Digest).With("given", cfg.Digest)
			}
			return nil // idempotent: same generation, same digest
		}
	}
	stored := cfg
	stored.ConfigBytes = slices.Clone(cfg.ConfigBytes) // defensive: own our bytes
	s.committed = &stored
	return nil
}

// CommittedConfig returns the last-committed config artifact, or ErrNotFound if
// nothing has been committed or seeded yet.
func (s *Store) CommittedConfig(ctx context.Context) (persistence.CommittedRolloutConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trace(ctx, "committed", 0)

	if s.committed == nil {
		return persistence.CommittedRolloutConfig{}, shared.ErrNotFound.WithMessage("no committed config artifact")
	}
	out := *s.committed
	out.ConfigBytes = slices.Clone(s.committed.ConfigBytes) // never hand back an aliased slice
	return out, nil
}

// mutate serializes a transition against the current rollout: it locates the
// rollout for generation, applies the domain transition fn, and installs the
// result as the new current state -- the whole sequence under the store mutex
// so the domain's invariant checks act as an atomic compare-and-set.
func (s *Store) mutate(
	ctx context.Context,
	generation uint64,
	op string,
	fn func(persistence.Rollout) (persistence.Rollout, *shared.BridgeError),
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trace(ctx, op, generation)

	r, err := s.rolloutForGen(generation)
	if err != nil {
		return err
	}
	next, berr := fn(r)
	if berr != nil {
		return berr
	}
	s.current = &next
	return nil
}

// rolloutForGen returns a copy of the current rollout iff it matches generation,
// else ErrNotFound (the referenced generation is not the active rollout).
// Callers must hold s.mu.
func (s *Store) rolloutForGen(generation uint64) (persistence.Rollout, *shared.BridgeError) {
	if s.current == nil || s.current.Generation() != generation {
		return persistence.Rollout{}, shared.ErrNotFound.
			WithMessage("no active rollout at generation").With("generation", generation)
	}
	return *s.current, nil
}

// trace emits a trace-level diagnostic when the logger is trace-enabled.
func (s *Store) trace(ctx context.Context, op string, generation uint64) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "memoryrollout: "+op, "generation", generation)
	}
}

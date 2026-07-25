package bridge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// The barrier drive: one goroutine per node that observes the rollout row on a
// cadence and acts on it (design §6). Every member runs the applier half;
// whichever member holds the coordinator lease additionally runs the coordinator
// half, so there is no separate coordinator process and no node-to-node RPC.
//
// The two halves share one ticker deliberately. They are both "re-read the
// rollout row and act", they must not interleave with each other on the same
// node (a coordinator that commits should apply on its own next observation like
// any other member), and one goroutine is one lifetime to reason about.

const (
	// defaultRolloutPollInterval is how often a member re-reads the rollout row.
	// Store notifications are hints, never truth (research rule 11), so the poll
	// IS the mechanism rather than a fallback. It is deliberately short relative
	// to the rollout TTL: staging latency is dominated by member build time, and
	// a cheap consistent read of one row is not a load concern.
	defaultRolloutPollInterval = 2 * time.Second

	// defaultCoordLeaseTTL bounds how long a crashed coordinator blocks its
	// successor (F3), and — because all coordinators share the TTL — doubles as
	// the Chubby-style lock delay a fresh coordinator waits out before its first
	// side effect (§6, firstSideEffectAllowed).
	defaultCoordLeaseTTL = 30 * time.Second
)

// startRolloutDrive launches the barrier drive for a coordinated deployment and
// returns a function that stops it and waits for the goroutine to exit. It
// returns nil when the barrier is not wired or the deployment is not coordinated
// — the drive is opt-in exactly like the barrier itself.
func (s *Supervisor) startRolloutDrive(ctx context.Context) func() {
	if s.rollout == nil || !coordinatedRollout(s.Config()) {
		return nil
	}
	obs := &rolloutObserver{metrics: s.metrics}
	s.mu.Lock()
	s.rolloutObs = obs
	s.mu.Unlock()

	applier := &rolloutApplier{sup: s, store: s.rollout.store, memberID: s.rollout.memberID, obs: obs}
	coord := newRolloutCoordinator(rolloutCoordinatorConfig{
		Store: s.rollout.store,
		Lease: s.rollout.lease,
		// The coordinator's live membership and the proposer's frozen epoch MUST
		// come from the same source or decideRollout reads a membership change
		// (F6) on every rollout. Both read bridge.cluster.members off the applied
		// config; Config() is re-read per observation so a whole-cohort roster
		// replacement is seen without a restart.
		Membership: func() []string { return rolloutMembers(s.Config()) },
		Clock:      s.clk,
		MemberID:   s.rollout.memberID,
		LeaseTTL:   s.rollout.leaseTTL,
		Logger:     s.logger,
	})

	loopCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.driveRollout(loopCtx, applier, coord)
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// driveRollout is the drive loop. It never returns an error: every failure is a
// store outage (F9) or a lost election, both of which are retried on the next
// tick while the running config keeps serving.
func (s *Supervisor) driveRollout(ctx context.Context, applier *rolloutApplier, coord *rolloutCoordinator) {
	ticker := s.clk.NewTicker(s.rollout.pollInterval)
	defer ticker.Stop()
	// Release the coordinator lease on the way out so a successor does not wait
	// out the full TTL after an orderly shutdown (F3 is the crash path; an
	// orderly stop should not pay it). Uses a context detached from the
	// cancelled loop ctx, which is why it is bounded by the lease TTL.
	defer coord.resign(context.WithoutCancel(ctx))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if err := applier.step(ctx); err != nil && s.logger != nil {
				// F9: the rollout resolves (or deadline-aborts) when the store
				// returns; nothing flipped, and this member keeps serving.
				s.logger.Warn("supervisor: cluster rollout applier observation failed; retrying",
					"error", err)
			}
			if err := coord.tick(ctx); err != nil && s.logger != nil {
				s.logger.Warn("supervisor: cluster rollout coordinator observation failed; retrying",
					"error", err)
			}
		}
	}
}

// tick performs one coordinator cycle: hold or win the coordinator lease, then
// take one fenced observation under it.
//
// Losing the election is the NORMAL state for all but one member and is not an
// error. A stale fencing token means this coordinator was deposed and the live
// one already decided (F5): it steps down and stops acting, rather than fighting
// a decision the store has already fenced.
func (c *rolloutCoordinator) tick(ctx context.Context) error {
	if !c.tok.Valid() {
		if err := c.elect(ctx); err != nil {
			if errors.Is(err, shared.ErrAlreadyExists) {
				return nil // another member coordinates; nothing to do
			}
			return err
		}
		// Elected. Take NO side effect on this tick: firstSideEffectAllowed
		// would reject it anyway (the lock delay starts now), and returning here
		// keeps the election observable as its own step.
		if c.logger != nil {
			c.logger.Info("supervisor: elected cluster rollout coordinator",
				"member", c.memberID, "lock_delay", c.leaseTTL)
		}
		return nil
	}
	// Renew before deciding so the token cannot lapse mid-decision. The port
	// contract guarantees Renew preserves the fencing Version established at
	// Acquire, so renewal never invalidates this coordinator's own decisions.
	tok, err := c.lease.Renew(ctx, coordLeaseID, c.tok, c.leaseTTL, nil)
	if err != nil {
		if errors.Is(err, shared.ErrStaleFencingToken) || errors.Is(err, shared.ErrNotFound) {
			// Genuinely deposed or expired: drop the token and let a successor
			// coordinate.
			c.stepDown("the coordinator lease was taken over or expired", err)
			return nil
		}
		// A TRANSIENT renewal failure (throttling, a store blip) must NOT step
		// down. Stepping down clears electedAt, so the next tick re-elects and
		// pays a fresh full lock delay before it may decide anything — one
		// throttled call would cost the cohort a whole leaseTTL of decision
		// latency, and a flaky store would starve every rollout into its
		// deadline. The lease is still held; keep the token and retry. If the
		// outage really outlives the TTL, a peer takes over and the NEXT renewal
		// returns a stale-token error, which is handled above.
		return err
	}
	c.tok = tok

	if _, err := c.observe(ctx); err != nil {
		if errors.Is(err, shared.ErrStaleFencingToken) {
			// F5: deposed AFTER the live coordinator already decided. The store
			// rejected the stale re-decision, which is the fence doing its job.
			c.stepDown("coordinator was deposed; the live coordinator has already decided", err)
			return nil
		}
		return err
	}
	return nil
}

// stepDown drops this coordinator's fencing token so the next tick re-elects
// from scratch. It does NOT release the lease: the token is stale precisely
// because someone else owns the lease now, and releasing under a stale token
// would be rejected anyway.
func (c *rolloutCoordinator) stepDown(reason string, err error) {
	if c.logger != nil && c.tok.Valid() {
		c.logger.Warn("supervisor: stepping down as cluster rollout coordinator",
			"member", c.memberID, "reason", reason, "error", err)
	}
	c.tok = persistence.LeaseToken{}
	c.electedAt = time.Time{}
}

// resign releases the coordinator lease on orderly shutdown so a successor can
// take over immediately instead of waiting out the TTL. Best-effort: a failure
// simply falls back to TTL expiry, which is the crash path (F3) and always safe.
func (c *rolloutCoordinator) resign(ctx context.Context) {
	if !c.tok.Valid() {
		return
	}
	relCtx, cancel := context.WithTimeout(ctx, c.leaseTTL)
	defer cancel()
	if err := c.lease.Release(relCtx, coordLeaseID, c.tok); err != nil && c.logger != nil {
		c.logger.Warn("supervisor: releasing the cluster rollout coordinator lease failed; a "+
			"successor takes over when the lease TTL expires", "error", err)
	}
	c.tok = persistence.LeaseToken{}
}

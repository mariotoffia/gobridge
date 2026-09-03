package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// The barrier drive: one goroutine per node that observes the rollout row on a
// cadence and acts on it (ADR 0013). Every member runs the applier half;
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
	// successor, and — because all coordinators share the TTL — doubles as
	// the Chubby-style lock delay a fresh coordinator waits out before its first
	// side effect (§6, firstSideEffectAllowed).
	defaultCoordLeaseTTL = 30 * time.Second

	// rolloutResignBudget bounds the orderly release of the coordinator lease on
	// shutdown. The release is a COURTESY — it saves a successor from waiting out
	// the TTL — so it may not be paid for out of the process shutdown budget: a
	// release bounded by the TTL itself would spend 30 seconds (or, in a
	// deployment that raised it, minutes) of SIGTERM on a store that has already
	// failed at the one thing this call does. TTL expiry is the fallback, and it
	// is the crash path, which is always safe.
	rolloutResignBudget = 5 * time.Second
)

// tick performs one coordinator cycle: hold or win the coordinator lease, then
// take one fenced observation under it.
//
// Losing the election is the NORMAL state for all but one member and is not an
// error. A stale fencing token means this coordinator was deposed and the live
// one already decided: it steps down and stops acting, rather than fighting
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
	// held is read into a local because a renewal that blows its budget is
	// abandoned and keeps running: a closure reading c.tok directly would race
	// this coordinator's own next write to it (rolloutOps.run).
	held, ttl := c.tok, c.leaseTTL
	tok, err := rolloutOpValue(ctx, c.ops, rolloutOpLease,
		func(callCtx context.Context) (persistence.LeaseToken, error) {
			return c.lease.Renew(callCtx, coordLeaseID, held, ttl, nil)
		})
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
			// deposed AFTER the live coordinator already decided. The store
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
// simply falls back to TTL expiry, which is the crash path and always safe —
// which is exactly why it gets the SHORTER of the lease TTL and the resignation
// budget rather than the whole TTL.
func (c *rolloutCoordinator) resign(ctx context.Context) {
	if !c.tok.Valid() {
		return
	}
	relCtx, cancel := context.WithTimeout(ctx,
		min(orDefault(c.leaseTTL, defaultCoordLeaseTTL), rolloutResignBudget))
	defer cancel()
	held := c.tok
	if err := c.ops.run(relCtx, rolloutOpLease, func(callCtx context.Context) error {
		return c.lease.Release(callCtx, coordLeaseID, held)
	}); err != nil && c.logger != nil {
		c.logger.Warn("supervisor: releasing the cluster rollout coordinator lease failed; a "+
			"successor takes over when the lease TTL expires", "error", err)
	}
	c.tok = persistence.LeaseToken{}
}

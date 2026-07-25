package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// coordLeaseID is the well-known lease that elects the rollout coordinator. Its
// LeaseToken is the fencing token passed to Commit/Abort (invariant I3), so
// election and fencing reuse the existing LeaseStore — no new election code.
const coordLeaseID = "cluster/rollout-coordinator"

// rolloutAction is the coordinator's verdict for a single observation of the
// rollout row. The coordinator is stateless across observations: decideRollout
// is a pure function of (rollout, live membership, now). A successor coordinator
// elected after a crash (F3) resumes by computing the same verdict from durable
// store state — recovery is resumption, not repair.
type rolloutAction int

const (
	// rolloutActionWait: the rollout is healthy but not yet decidable, or it is
	// already terminal — the coordinator takes no side effect this observation.
	rolloutActionWait rolloutAction = iota
	// rolloutActionCommit: every epoch member acked — flip to Committed.
	rolloutActionCommit
	// rolloutActionAbort: a Nack, the deadline, or a membership change makes the
	// barrier unsatisfiable — flip to Aborted with the returned reason.
	rolloutActionAbort
)

// decideRollout computes the coordinator's next action for one observation.
// Abort conditions are evaluated before the commit check so a rollout that is
// simultaneously complete-and-invalid (all acked, but membership changed under
// it) aborts rather than commits. The commit check precedes the deadline check
// so a fully-acked rollout is never wasted merely because the clock crossed the
// deadline in the same tick.
func decideRollout(r persistence.Rollout, membership []string, now time.Time) (rolloutAction, string) {
	if r.State().IsTerminal() {
		return rolloutActionWait, ""
	}
	// F6: the epoch is frozen at Propose; any live-membership divergence — a
	// joiner or a departure — makes strict all-ack unachievable, so abort. The
	// live roster is a SET: sort AND dedup it before comparing to the frozen
	// epoch (which NewRollout already sorted and deduped), so a transient
	// duplicate member id from the membership source is not misread as a change.
	epoch := sortedSet(membership)
	if !slices.Equal(epoch, r.MembershipEpoch()) {
		return rolloutActionAbort, fmt.Sprintf(
			"membership changed during rollout: epoch=%v now=%v", r.MembershipEpoch(), epoch)
	}
	// F2: any Nack aborts immediately, without waiting out the deadline.
	if reason := aggregateNacks(r.Nacks()); reason != "" {
		return rolloutActionAbort, reason
	}
	// Happy path: the whole epoch acked → commit.
	if r.CanCommit() {
		return rolloutActionCommit, ""
	}
	// F1: an incomplete rollout that outran its deadline aborts; survivors keep
	// the old committed config and a crashed member rejoins on it.
	if now.After(r.Deadline()) {
		return rolloutActionAbort, fmt.Sprintf(
			"rollout deadline exceeded with %d/%d acks", len(r.Acks()), len(r.MembershipEpoch()))
	}
	return rolloutActionWait, ""
}

// rolloutCoordinatorConfig wires a rolloutCoordinator to its collaborators.
// Membership is injected as a function so Phase 3 can unit-test on a fake roster;
// its real source (heartbeat rows vs lease endpoints, open question Q1) lands
// with the operator surface in a later phase.
type rolloutCoordinatorConfig struct {
	Store      ports.ClusterRolloutStore
	Lease      ports.LeaseStore
	Membership func() []string
	Clock      clock.Clock
	MemberID   string
	LeaseTTL   time.Duration
	Logger     *slog.Logger
}

// rolloutCoordinator drives an active rollout to a terminal decision while it
// holds the coordinator lease. It is deliberately caller-driven: elect() once,
// then observe() on a cadence (the Run loop, wired in the supervisor). All
// durable state lives in the store, so a successor coordinator resumes by
// electing and observing — recovery is resumption, not repair (F3).
type rolloutCoordinator struct {
	store      ports.ClusterRolloutStore
	lease      ports.LeaseStore
	membership func() []string
	clk        clock.Clock
	memberID   string
	leaseTTL   time.Duration
	logger     *slog.Logger

	tok       persistence.LeaseToken
	electedAt time.Time
}

// newRolloutCoordinator builds a coordinator, defaulting the clock and logger.
func newRolloutCoordinator(cfg rolloutCoordinatorConfig) *rolloutCoordinator {
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &rolloutCoordinator{
		store:      cfg.Store,
		lease:      cfg.Lease,
		membership: cfg.Membership,
		clk:        clk,
		memberID:   cfg.MemberID,
		leaseTTL:   cfg.LeaseTTL,
		logger:     logger,
	}
}

// elect acquires the coordinator lease and records the election instant. The
// returned LeaseToken is this coordinator's fencing token for the rollout.
func (c *rolloutCoordinator) elect(ctx context.Context) error {
	tok, err := c.lease.Acquire(ctx, coordLeaseID, c.memberID, c.leaseTTL, nil)
	if err != nil {
		return err
	}
	c.tok = tok
	c.electedAt = c.clk.Now()
	return nil
}

// observe performs one gated observation. During the lock-delay window it takes
// no side effect (a freshly elected coordinator waits out one previous-lease
// duration so a slow predecessor can notice its lease lapsed); afterwards it
// delegates to the fenced coordinatorStep. Returns terminal=true once decided.
func (c *rolloutCoordinator) observe(ctx context.Context) (bool, error) {
	if !c.tok.Valid() {
		return false, nil // not elected: hold no fencing token, take no side effect
	}
	if !firstSideEffectAllowed(c.electedAt, c.leaseTTL, c.clk.Now()) {
		return false, nil // lock-delay: observe only, no decision yet
	}
	return coordinatorStep(ctx, c.store, c.membership(), c.tok, c.clk.Now())
}

// firstSideEffectAllowed reports whether a coordinator elected at electedAt has
// waited out its lock-delay by now. lockDelay is one previous-lease duration
// (all coordinators share the lease TTL, so the TTL is the delay). Chubby-style
// belt-and-braces over the fencing epoch (design §6).
func firstSideEffectAllowed(electedAt time.Time, lockDelay time.Duration, now time.Time) bool {
	return !now.Before(electedAt.Add(lockDelay))
}

// coordinatorStep performs one fenced observe→act cycle for the elected
// coordinator. It re-reads the rollout through the store (ConsistentRead — a
// store notification is a hint, never truth: research rule 11), computes the
// verdict with decideRollout, and executes it under the coordinator's fencing
// token.
//
// terminal is true only once the rollout is decided, so the Run loop can stop.
// A store error is returned with terminal=false and no state flipped: an outage
// (F9) is retried by the loop; a stale-token rejection because this coordinator
// was deposed AFTER the live one already decided (F5) is surfaced so the loop
// steps down. An empty store (no rollout proposed) is a benign no-op.
//
// # F5b — first-decision fencing: DECIDED, residual accepted
//
// The store fence only rejects a stale RE-decision (see
// persistence.Rollout.coordVersion), so a deposed coordinator that decides
// FIRST is not rejected. The design (§7 F5b) left open whether to close that
// with a coordinator-claim write — one extra write per election that stamps the
// live epoch on the rollout row before any decision. It is deliberately NOT
// added, because every reachable outcome of the residual is already fail-safe
// and the window is bounded three times over:
//
//  1. A zombie COMMIT still has to satisfy the full ack barrier (I2). If every
//     epoch member acked, committing is the CORRECT outcome — the only
//     irregularity is which process wrote it.
//  2. A zombie ABORT just keeps the old config serving. The operator retries;
//     nothing swapped, nothing was lost.
//  3. The window itself: rolloutCoordinator.tick RENEWS the lease before every
//     observation, so a deposed coordinator's renewal is rejected and it steps
//     down before reaching a decision. Only a takeover landing between a
//     successful renewal and the decision write can slip through, and the
//     successor's lock delay (firstSideEffectAllowed, one full lease TTL) means
//     it is not racing a live decision even then.
//
// A claim write would trade a per-election write and a new failure mode
// (a claim that succeeds while the decision does not) for closing a window whose
// every outcome is already safe. If the confirm window (§8.1, Phase 7) ever
// makes a decision externally visible BEFORE the barrier is satisfied, revisit:
// that is the first change that would make a zombie decision observable.
func coordinatorStep(
	ctx context.Context,
	store ports.ClusterRolloutStore,
	membership []string,
	tok persistence.LeaseToken,
	now time.Time,
) (bool, error) {
	r, err := store.Current(ctx)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return false, nil // no rollout proposed yet — nothing to drive
		}
		return false, err // F9: outage — no decision this cycle, loop retries
	}
	action, reason := decideRollout(r, membership, now)
	switch action {
	case rolloutActionCommit:
		if err := store.Commit(ctx, r.Generation(), tok); err != nil {
			return false, err // F5: deposed coordinator → stale token rejected
		}
		return true, nil
	case rolloutActionAbort:
		if err := store.Abort(ctx, r.Generation(), tok, reason); err != nil {
			return false, err
		}
		return true, nil
	default:
		// rolloutActionWait covers both healthy-but-undecided and already
		// terminal; only the latter stops the loop.
		return r.State().IsTerminal(), nil
	}
}

// aggregateNacks renders the nack set (member→reason) into one deterministic,
// operator-facing string, or "" when there are no nacks. Members are sorted so
// the abort reason is stable regardless of map iteration order.
func aggregateNacks(nacks map[string]string) string {
	if len(nacks) == 0 {
		return ""
	}
	members := make([]string, 0, len(nacks))
	for m := range nacks {
		members = append(members, m)
	}
	slices.Sort(members)
	parts := make([]string, 0, len(members))
	for _, m := range members {
		parts = append(parts, fmt.Sprintf("%s nacked: %s", m, nacks[m]))
	}
	return strings.Join(parts, "; ")
}

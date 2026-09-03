package bridge

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Coordinated cluster rollout — the applier half of the barrier (ADR 0013).
//
// # Candidate transport (ADR 0013 open item — DECIDED here: option (b))
//
// §5 sketched having the proposer write the candidate INACTIVE to the config
// source and every member fetch it back by digest (option (a)). That is not what
// this implementation does, for two reasons:
//
//  1. The DynamoDB config source is a single `current` slot
//     (adapters/aws/config/dynamodb/loader.go: skCurrent), so an inactive row
//     would be new machinery — a staged sort key, a new fetch port, a
//     promote-on-commit writer, and a drop-on-abort writer.
//  2. Every member ALREADY receives the candidate through its own config
//     watcher; that is how the proposer half works (rollout_barrier.go). The
//     rollout row's digest is the cross-member agreement check: a member whose
//     source handed it different bytes computes a different digest and Nacks
//     so a divergent config aborts the rollout instead of splitting the
//     cohort.
//
// So each member STAGES the candidate its own source delivered
// (rolloutBarrier.stage) and the applier builds from that. The safety hole §5
// named for option (b) — `current` is written before the barrier decides, so a
// member restarting after an ABORTED rollout would boot the rejected config
// while the cohort keeps the old one, which is a member running a generation the
// cohort never agreed on — is closed by the joiner rule in rollout_joiner.go,
// NOT left open. (That guarantee is absolute; the per-member convergence window
// AFTER a commit is the separate, bounded one — see ADR 0013.)
//
// A consequence worth stating: there are no FETCHED candidate bytes, so the
// design's "verify the digest before decoding hostile input" obligation is met
// by construction. The bytes were decoded by this node's own config-source load
// path (the same validated path a single-node reload uses) before any rollout
// row was consulted; the digest check is a cross-member AGREEMENT check, not a
// trust boundary.

// rolloutApplier is the per-node half of the barrier: it observes the rollout
// row, votes on a proposal (build-without-swap, then Ack or Nack), and performs
// the local swap once the coordinator commits.
//
// It is caller-driven — one step() per observation — so unit tests drive it
// deterministically without a goroutine or a clock; Supervisor.Run wires the
// cadence (rollout_loop.go).
type rolloutApplier struct {
	// host is the runtime the applier votes for and swaps; barrier is the shared
	// proposer/candidate/committed-artifact state. The two were once a single
	// *Supervisor field; splitting them is what lets bootstrap.App host the same
	// applier as the Supervisor.
	host     ports.RolloutHost
	barrier  *rolloutBarrier
	store    ports.ClusterRolloutStore
	memberID string
	gate     nodeRolloutGate
	obs      *rolloutObserver
	// clk stamps the confirm-window deadman check (ADR 0014). System clock in
	// production; a fake in tests. Only read from the drive goroutine.
	clk clock.Clock

	// Confirm-window provisional state (ADR 0014). All touched only from the
	// drive goroutine, so no lock is needed.
	//
	//   - provisionalGen: the generation this member provisionally swapped to in
	//     THIS process (0 = none). It gates the revert: only a member that actually
	//     applied the provisional generation has anything to undo.
	//   - revertCfg: the config this member was running just BEFORE the provisional
	//     swap (i.e. generation N-1). It is the revert target — cached rather than
	//     re-fetched from the committed artifact because the FIRST windowed rollout
	//     has no prior committed artifact, and the running config is always N-1.
	//   - convergeSent: whether this member already wrote its Converge for
	//     provisionalGen (convergence is unretractable, — write it once).
	//   - revertedGen: a generation this member already reverted locally (deadman or
	//     an observed Revert), so it does not re-apply or re-converge it.
	provisionalGen uint64
	revertCfg      *ports.BridgeConfig
	convergeSent   bool
	revertedGen    uint64
	// provisionalDeadline is when this member's LOCAL confirm-window deadman
	// fires: the observed confirm deadline plus the grace ticks. It is cached at
	// the provisional swap so the deadman needs no store read to run — the
	// store outage and the dead coordinator it exists for are the same outage.
	provisionalDeadline time.Time
	// artifactGen is the generation whose durable committed artifact this member
	// has VERIFIED as written, so it does not re-write it every poll of the
	// terminal Confirmed row. It advances only after a read-back check, never on
	// the strength of a write that merely reported success.
	artifactGen uint64
	// applyRepair, pendingArtifact and pendingRevert are the three local safety
	// operations this member owes and has not completed: reaching the generation
	// the cohort decided on, recording the committed artifact, and getting back to
	// the last confirmed generation. All three retry and end in terminalGen when
	// the bound is reached (rollout_recovery.go). applyRepair paces only AFTER the
	// bound: the first attempts chase a transient cause and want the drive's own
	// cadence, while past the bound the cause is deterministic and the capped
	// backoff is what keeps a doomed swap from rebuilding the runtime every poll.
	applyRepair *rolloutRepair
	// attemptedGen is the generation applyCommittedGeneration attempted during the
	// CURRENT observation; step resets it. See step for why.
	attemptedGen    uint64
	pendingArtifact *rolloutRepair
	pendingRevert   *rolloutRepair
	// terminalGen is a generation whose SAFE state this member could not reach
	// within the bounded retries, and terminalClass which operation could not
	// reach it. It is a latch: the member cannot get itself back to a state
	// consistent with the cohort's decision and needs operator attention. WHICH
	// attention differs by cause, and the reason carries it — see markUnsafe. The
	// class is what lets a completing repair retract only ITS OWN latch.
	terminalGen   uint64
	terminalClass string
}

// step performs one observation of the rollout row and acts on it. Store
// notifications are hints, never truth: every decision re-reads the row through
// the store, whose implementations read consistently (research rule 11).
//
// A returned error is a STORE error only — the loop logs and retries it (an
// outage flips no state, and members keep serving the old config). Every
// protocol outcome (Nack, no candidate yet, not our cohort) is a normal return.
func (a *rolloutApplier) step(ctx context.Context) error {
	// One apply attempt per observation: adopt and the reconcile fallback below
	// both drive applyCommittedGeneration, and letting both attempt the same
	// generation back-to-back would burn two retries on one transient failure.
	// Recorded by the attempt itself, so a path that did NOT attempt (a member
	// that never staged the candidate) leaves the fallback free to converge it
	// from the durable artifact.
	a.attemptedGen = 0
	r, err := rolloutOpValue(ctx, a.ops(), rolloutOpRead, a.store.Current)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			// No active rollout. That IS an observation of the shared row — the
			// answer is "nothing in flight" — so it keeps the freshness signal alive
			// through the long stretches between rollouts. The durable artifact may
			// still be ahead of this member (a commit it missed whose row was later
			// cleared).
			a.obs.observeAbsent()
			return a.reconcileMissedCommit(ctx, 0)
		}
		return err
	}
	// Publish BEFORE acting, so the observation an operator reads is what this
	// member saw and reacted to — including the "staged" flag that identifies a
	// member whose own config source has not delivered the candidate, which is
	// otherwise invisible and is the most common reason a rollout hangs.
	if a.obs != nil {
		staged, applied := a.observedState(r)
		a.obs.observe(r, a.memberID, staged, applied)
	}
	// decidedGen is the generation the observed row has DECIDED, and zero while it
	// has decided nothing. The reconcile fallback below must not converge to a
	// durable artifact older than it: that generation is superseded, and the two
	// paths would reset each other's bounded retry.
	var decidedGen uint64
	switch r.State() {
	case persistence.RolloutProposed, persistence.RolloutStaging:
		if err := a.vote(ctx, r); err != nil {
			return err
		}
	case persistence.RolloutCommitted:
		decidedGen = r.Generation()
		if r.ConfirmDeadline().IsZero() {
			// Base protocol: final commit — swap and advance the committed artifact.
			if err := a.adopt(ctx, r); err != nil {
				return err
			}
		} else {
			// Confirm window (ADR 0014): provisional swap, converge, deadman revert.
			if err := a.adoptProvisional(ctx, r); err != nil {
				return err
			}
		}
	case persistence.RolloutConfirmed:
		decidedGen = r.Generation()
		if err := a.adoptConfirmed(ctx, r); err != nil {
			return err
		}
	case persistence.RolloutReverted:
		a.revertProvisional(ctx, r)
	default:
		// Aborted. Nothing to discard: the candidate build is released the
		// instant it has proven itself (see vote), so an abort costs this node
		// no cleanup — the candidate-cleanup obligation is discharged
		// at proof time rather than deferred across the whole staging window.
	}
	// Fallback: converge to the durable last-committed artifact if it is AHEAD of
	// what this member applied — a commit it missed because the active row moved on
	// (residual seq (2)). Runs AFTER adopt so the normal path (which applies with
	// the staged candidate's source pointer) wins and this is a no-op then.
	return a.reconcileMissedCommit(ctx, decidedGen)
}

// observedState answers the two questions the observation publishes about THIS
// member: does it hold the candidate its own config source must deliver, and is
// it running the generation's config.
//
// "applied" is answered against the DIGEST the cohort agreed on, which is the
// only answer that holds in every shape this member can be in. The alternatives
// each have a hole, and both holes report a correctly-converged member as
// diverged — a fleet alarm firing on a healthy cohort:
//
//   - the local applied-generation gate is in-memory, so a member that RESTARTED
//     onto the committed artifact runs the generation with an empty gate. It is
//     also not seeded at boot, and a deployment that wires no config codec never
//     advances it at all, so there the answer would be "not applied" forever.
//   - the staged candidate is absent for a member that was down when its config
//     source delivered the change. That member converges through the durable
//     artifact, which it decodes and applies without ever staging anything.
//
// The running config's canonical digest has none of that history in it: it is
// what this member is running, right now, compared against what the cohort
// agreed to run. It costs one canonicalisation per poll. The gate is the
// fallback for the one config that cannot be canonicalised at all, which no
// config loaded through the normal path can be.
func (a *rolloutApplier) observedState(r persistence.Rollout) (staged, applied bool) {
	_, staged = a.barrier.candidate(r.ConfigDigest())
	digest, ok := configCanonicalBytesDigest(a.host.Config())
	if !ok {
		return staged, a.gate.applied >= r.Generation()
	}
	return staged, digest == r.ConfigDigest()
}

// vote runs the pre-build gate for an undecided rollout and records this node's
// verdict exactly once.
//
// The build is a PROOF, not a staged runtime: Builder.Plan runs the same prepare
// phase a real reload runs (validate, build stores, assemble options — no
// transport sessions, which belong to Commit), and its plan is released
// immediately. Holding it open until the coordinator decides would pin the
// candidate's store handles — SQLite file locks, DynamoDB clients — alongside
// the running runtime's for the entire staging window, which can be minutes.
// The cost is that a committed rollout re-runs prepare during the real swap;
// that is one extra prepare per rollout, and it buys the swap the exact,
// already-hardened apply path instead of a parallel one.
//
// # What an Ack does and does not prove
//
// It proves the candidate passes the wired ports.BlueprintValidator and that
// this member can build its stores and runtime options. It does NOT prove the
// transports connect (they open in the commit phase — §8.1's Model A, and why
// alarms rather than rolls back), and it does not prove the runtime's
// route-graph validation, which also runs in the commit phase.
//
// That last one makes the validator load-bearing for the barrier, not merely for
// config hygiene: WITHOUT one, Builder.Plan trusts the input (see its doc), so a
// dangling reference is acked by every member, committed, and then fails every
// member's swap. The cohort stays consistent — all members fail identically and
// recover the old config, so holds — but the change costs a cohort-wide
// failed swap instead of an abort. A coordinated deployment should always wire
// config.Validate.
func (a *rolloutApplier) vote(ctx context.Context, r persistence.Rollout) error {
	if !slices.Contains(r.MembershipEpoch(), a.memberID) {
		// Not a voter in this epoch: the aggregate would reject the vote, so
		// abstaining is correct. Say so anyway. The cohort's only other outcome
		// here is a deadline abort, and an abort whose cause is "a member the
		// roster does not list was waiting to vote" leaves no trace on the one node
		// an operator actually logs into. Naming this member and the roster it is
		// missing from IS the diagnosis — a config that lists the wrong members, or
		// a member started under the wrong identity.
		if a.host.RolloutLogger() != nil {
			a.host.RolloutLogger().Warn("supervisor: abstaining from the coordinated cluster rollout vote; "+
				"this member is not in the rollout's frozen membership epoch, so it cannot vote and the "+
				"cohort will never count it. Check bridge.cluster.members against this member's identity",
				"generation", r.Generation(), "config_version", r.ConfigVersion(),
				"member", a.memberID, "epoch", r.MembershipEpoch())
		}
		return nil
	}
	if _, acked := r.Acks()[a.memberID]; acked {
		return nil
	}
	if _, nacked := r.Nacks()[a.memberID]; nacked {
		return nil
	}
	cand, staged := a.barrier.candidate(r.ConfigDigest())
	if !staged {
		// This node's own config source has not delivered the candidate yet.
		// Staying silent is correct: the coordinator's deadline bounds the
		// wait and aborts if it never arrives, and a member that guessed instead
		// would break the agreement the digest exists to prove.
		return nil
	}
	raw, ok := configCanonicalBytes(cand.frozen)
	if !ok {
		return a.nack(ctx, r, "candidate config could not be canonicalised for digest verification")
	}
	if reason := evaluateProposal(a.host.Config(), cand.frozen, raw, r.ConfigDigest()); reason != "" {
		return a.nack(ctx, r, reason)
	}
	release, err := a.host.PlanCandidate(ctx, cand.frozen)
	if err != nil {
		// A Nack is a PERMANENT, unretryable verdict: the aggregate admits one
		// vote per member per generation and the coordinator aborts on the
		// first one. It must therefore mean "this config is wrong", never
		// "I was briefly unable to check". PlanCandidate opens stores and resolves
		// credentials, so a throttled store, a flaky credential provider, or —
		// because ctx is the drive loop's — an ordinary SIGTERM would otherwise
		// let one restarting member poison every in-flight rollout in the cohort.
		// Abstain instead and let the deadline decide; that outcome is
		// recoverable by re-proposing, a nack is not.
		if transientBuildFailure(ctx, err) {
			if a.host.RolloutLogger() != nil {
				a.host.RolloutLogger().Warn("cluster rollout: abstaining from the vote; the candidate "+
					"build failed for a transient reason and a Nack would permanently abort a "+
					"recoverable rollout", "generation", r.Generation(), "error", err)
			}
			return nil
		}
		return a.nack(ctx, r, "candidate build failed on this member: "+err.Error())
	}
	release()

	// The build digest proves WHAT was built. Every member builds from the
	// digest-verified candidate, so it equals the candidate digest by
	// construction; recording it keeps the ack self-describing in the audit
	// trail and satisfies the aggregate's non-empty requirement.
	if err := a.ops().run(ctx, rolloutOpVote, func(callCtx context.Context) error {
		return a.store.Ack(callCtx, r.Generation(), a.memberID, r.ConfigDigest())
	}); err != nil {
		// A rollout that resolved (or was superseded) between the read and the
		// vote rejects it. That is the barrier working, not a store outage:
		// swallow it so the loop does not log an outage that did not happen.
		if isRolloutVoteRejection(err) {
			return nil
		}
		return err
	}
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Info("supervisor: acked coordinated cluster rollout candidate",
			"generation", r.Generation(), "config_version", r.ConfigVersion(), "member", a.memberID)
	}
	return nil
}

// nack records this node's rejection with an operator-facing reason. The rollout
// does NOT terminate here — the coordinator observes the nack and aborts,
// so the decision stays with the single fenced decider.
func (a *rolloutApplier) nack(ctx context.Context, r persistence.Rollout, reason string) error {
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Error("supervisor: nacking coordinated cluster rollout candidate; the "+
			"coordinator aborts the rollout and every member keeps its running config",
			"generation", r.Generation(), "config_version", r.ConfigVersion(), "reason", reason)
	}
	if err := a.ops().run(ctx, rolloutOpVote, func(callCtx context.Context) error {
		return a.store.Nack(callCtx, r.Generation(), a.memberID, reason)
	}); err != nil {
		if isRolloutVoteRejection(err) {
			return nil
		}
		return err
	}
	return nil
}

// adopt performs the local swap for a committed rollout — the only path by which
// a coordinated member changes config, and only ever AFTER the store-atomic
// Commit that required every epoch member's ack.
func (a *rolloutApplier) adopt(ctx context.Context, r persistence.Rollout) error {
	if !a.gate.admits(r.Generation()) {
		return nil
	}
	cand, staged := a.barrier.candidate(r.ConfigDigest())
	if !staged {
		// Committed, but this node never staged the candidate — it was down when
		// its config source delivered it, or it joined after the commit. It catches
		// up through the durable committed artifact instead (reconcileMissedCommit,
		// run each step), which fetches the committed BYTES so no staged candidate
		// is needed. Do NOT record the generation here: reconcile owns it.
		return nil
	}
	if a.applyCommittedGeneration(ctx, r.Generation(), r.ConfigVersion(), a.adoptable(cand), cand.frozen) {
		// Only a member that actually applied the generation records the durable
		// artifact — its running config now IS the committed one.
		a.requireCommittedArtifact(ctx, r.Generation(), r.ConfigVersion(), cand.frozen)
	}
	return nil
}

// transientBuildFailure reports whether a candidate build failed for a reason
// that says nothing about the CANDIDATE — shutdown, a cancelled or expired
// context, or a throttled/unavailable dependency. Those must abstain, not cast
// the permanent Nack that aborts the whole cohort's rollout.
//
// It deliberately errs toward nacking: only positively-identified transient
// causes abstain, so a genuinely bad config is still rejected promptly rather
// than left to burn the full deadline. The ctx check catches a cancellation a
// builder wrapped without preserving the sentinel.
func transientBuildFailure(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, shared.ErrThrottled) ||
		errors.Is(err, shared.ErrUnavailable)
}

// isRolloutVoteRejection reports whether err means "this vote no longer applies"
// rather than "the store is unavailable". Both terminal-state and
// superseded-generation rejections are normal races between an observation and
// the vote it triggered.
func isRolloutVoteRejection(err error) bool {
	return errors.Is(err, shared.ErrNotFound) ||
		errors.Is(err, shared.ErrRolloutTerminal) ||
		errors.Is(err, shared.ErrRolloutAckRejected)
}

package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Coordinated cluster rollout — the applier half of the barrier (design §6).
//
// # Candidate transport (design §5 open item — DECIDED here: option (b))
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
// while the cohort keeps the old one (a mixed-version cohort, which forbids)
// — is closed by the joiner rule in rollout_joiner.go, NOT left open.
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
// maxAdoptAttempts bounds how many times a member retries applying a committed
// generation before it gives up and reports the divergence instead. Retrying is
// what converges a member whose swap failed transiently; NOT bounding it would
// rebuild the runtime every poll interval forever when the cause is
// deterministic, which is an outage rather than a recovery.
const maxAdoptAttempts = 3

type rolloutApplier struct {
	// host is the runtime the applier votes for and swaps; barrier is the shared
	// proposer/candidate/committed-artifact state. The two were once a single
	// *Supervisor field; splitting them is what lets bootstrap.App host the same
	// applier as the Supervisor (design Phase 6).
	host     ports.RolloutHost
	barrier  *rolloutBarrier
	store    ports.ClusterRolloutStore
	memberID string
	gate     nodeRolloutGate
	obs      *rolloutObserver
	// clk stamps the confirm-window deadman check (design §8.1). System clock in
	// production; a fake in tests. Only read from the drive goroutine.
	clk clock.Clock

	// attemptGen/attempts count consecutive apply attempts for one committed
	// generation. Both are touched only from the drive goroutine.
	attemptGen uint64
	attempts   int

	// Confirm-window provisional state (design §8.1). All touched only from the
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
	// pendingArtifact and pendingRevert are the two local safety operations this
	// member owes and has not completed: recording the committed artifact, and
	// getting back to the last confirmed generation. Both retry under a bounded
	// backoff and end in terminalGen when the bound is reached (rollout_recovery.go).
	pendingArtifact *rolloutRepair
	pendingRevert   *rolloutRepair
	// terminalGen is a generation whose SAFE state this member could not reach
	// within the bounded retries. It is a latch: the member cannot get itself back
	// to a state consistent with the cohort's decision and needs operator
	// attention. WHICH attention differs by cause, and the reason carries it —
	// see markUnsafe.
	terminalGen uint64
	// gaveUpGen is the generation whose swap this member gave up on after the bounded
	// retries. The base adopt path gets this bound from its gate guard; the confirm-
	// window paths do not consult the gate (they must keep running convergence /
	// deadman logic after a successful swap), so this latch is what stops them
	// rebuilding the runtime every poll when a swap deterministically fails.
	gaveUpGen uint64
}

// step performs one observation of the rollout row and acts on it. Store
// notifications are hints, never truth: every decision re-reads the row through
// the store, whose implementations read consistently (research rule 11).
//
// A returned error is a STORE error only — the loop logs and retries it (an
// outage flips no state, and members keep serving the old config). Every
// protocol outcome (Nack, no candidate yet, not our cohort) is a normal return.
func (a *rolloutApplier) step(ctx context.Context) error {
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
		cand, staged := a.barrier.candidate(r.ConfigDigest())
		// "applied" is answered by CONTENT, not by the local gate: after a
		// restart the gate is empty even though this member is running the
		// generation, and after a failed swap the gate may be set even though it
		// is not. Content is the only answer an operator can act on.
		applied := staged && configContentEqual(a.host.Config(), cand.frozen)
		a.obs.observe(r, a.memberID, staged, applied)
	}
	// adoptGen is the generation adopt already handled this step (the active
	// committed rollout). reconcile must not re-drive it in the same step or it
	// would burn a second retry attempt back-to-back on a transient failure.
	var adoptGen uint64
	switch r.State() {
	case persistence.RolloutProposed, persistence.RolloutStaging:
		if err := a.vote(ctx, r); err != nil {
			return err
		}
	case persistence.RolloutCommitted:
		adoptGen = r.Generation()
		if r.ConfirmDeadline().IsZero() {
			// Base protocol: final commit — swap and advance the committed artifact.
			if err := a.adopt(ctx, r); err != nil {
				return err
			}
		} else {
			// Confirm window (design §8.1): provisional swap, converge, deadman revert.
			if err := a.adoptProvisional(ctx, r); err != nil {
				return err
			}
		}
	case persistence.RolloutConfirmed:
		adoptGen = r.Generation()
		if err := a.adoptConfirmed(ctx, r); err != nil {
			return err
		}
	case persistence.RolloutReverted:
		adoptGen = r.Generation()
		a.revertProvisional(ctx, r)
	default:
		// Aborted. Nothing to discard: the candidate build is released the
		// instant it has proven itself (see vote), so an abort costs this node
		// no cleanup — RECONFIG-2's candidate-cleanup obligation is discharged
		// at proof time rather than deferred across the whole staging window.
	}
	// Fallback: converge to the durable last-committed artifact if it is AHEAD of
	// what this member applied — a commit it missed because the active row moved on
	// (residual seq (2)). Runs AFTER adopt so the normal path (which applies with
	// the staged candidate's source pointer) wins and this is a no-op then.
	return a.reconcileMissedCommit(ctx, adoptGen)
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
		return nil // not a voter in this epoch; the aggregate would reject us
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

// applyCommittedGeneration swaps the runtime to a committed generation and owns
// the bounded-retry / give-up-degraded behaviour shared by adopt (a staged
// candidate) and reconcileMissedCommit (config decoded from the durable
// artifact). apply is the pointer handed to applyBarrierCommitted (adopt passes
// the source pointer so the manager correlates the swap; reconcile has only the
// decoded config); content is the agreed content the swap is verified against. It
// returns true once this member is running that content.
func (a *rolloutApplier) applyCommittedGeneration(ctx context.Context, gen uint64, version int, apply, content *ports.BridgeConfig) bool {
	// Restart-safe idempotence without a durable counter: a node that already runs
	// this generation's content must not rebuild its runtime just because its
	// in-memory gate was reset by the restart.
	if configContentEqual(a.host.Config(), content) {
		a.gate.record(gen)
		return true
	}
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Info("supervisor: coordinated cluster rollout committed; applying the candidate",
			"generation", gen, "config_version", version)
	}
	if a.attemptGen != gen {
		a.attemptGen, a.attempts = gen, 0
	}
	a.attempts++
	a.host.ApplyCommitted(ctx, apply)

	// Did the swap actually take? applyConfig publishes the new config only on a
	// path that adopted it (including the admin-paused branch, which records it
	// for resume) and restores the OLD config when the swap fails. Comparing
	// content is therefore the honest answer, and it matters: the barrier has
	// already committed cluster-wide, so a member that silently fails to apply
	// leaves the mixed-version cohort forbids.
	if configContentEqual(a.host.Config(), content) {
		a.gate.record(gen)
		a.obs.observeRetry(rolloutRepairApply, 0)
		return true
	}
	// The swap failed. It recovered the old config, counted a reload failure, and
	// latched degraded — but on its own that leaves this member on the previous
	// generation while its peers moved on. Retry a bounded number of times: the
	// common causes (a broker refusing a connection, a store briefly unopenable)
	// are transient, and a retry genuinely converges the cohort.
	if a.attempts < maxAdoptAttempts {
		a.obs.observeRetry(rolloutRepairApply, a.attempts)
		if a.host.RolloutLogger() != nil {
			a.host.RolloutLogger().Error("supervisor: applying the committed cluster rollout failed; retrying "+
				"(this member runs an OLDER generation than the cohort until it succeeds)",
				"generation", gen, "attempt", a.attempts, "max_attempts", maxAdoptAttempts)
		}
		return false
	}
	// Out of retries: stop rebuilding the runtime in a loop and leave the
	// divergence loudly visible instead. RolloutStatus reports applied=false and
	// the supervisor stays degraded, so an operator sees WHICH member diverged
	// rather than a cohort that merely looks committed everywhere. gaveUpGen latches
	// the give-up so the confirm-window paths (which do not consult the gate) also
	// stop rebuilding.
	a.gate.record(gen)
	a.gaveUpGen = gen
	a.obs.observeRetry(rolloutRepairApply, 0)
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Error("supervisor: giving up applying the committed cluster rollout after repeated "+
			"failures; THIS MEMBER RUNS AN OLDER CONFIG GENERATION THAN THE COHORT. Investigate this "+
			"node and restart it once the cause is fixed",
			"generation", gen, "config_version", version, "attempts", a.attempts)
	}
	a.host.MarkDegraded(fmt.Sprintf("coordinated cluster rollout generation %d committed but could not "+
		"be applied on this member after %d attempts; it runs an older config generation than the rest "+
		"of the cohort", gen, a.attempts))
	return false
}

// adoptable picks which of the staged pointers to apply.
//
// Normally the SOURCE pointer: the config manager correlates apply results by
// exact pointer identity, so a swap event carrying the frozen clone would be
// discarded as foreign and leave the desired-vs-running divergence signal
// (ReconfigurePending, and deep health's Degraded) latched true forever after a
// perfectly successful rollout. applyConfig re-freezes whatever it is given, and
// freezing is content-preserving, so the build normally sees exactly the content
// the cohort agreed on.
//
// "Normally" is doing real work there. The re-freeze reads the source pointer
// AGAIN, at commit time, while the digest the cohort agreed on was computed from
// the frozen snapshot taken at propose time. Configs are immutable by contract
// once emitted, so the two cannot diverge — but this barrier exists precisely to
// guarantee every member applies the same bytes, and "by contract" is not a
// guarantee. If they ever diverge, apply the AGREED content and accept the lost
// correlation: a stale divergence gauge is an operator annoyance, whereas
// applying un-agreed content is the split-brain the whole protocol prevents.
func (a *rolloutApplier) adoptable(cand stagedCandidate) *ports.BridgeConfig {
	if cand.source == nil || !configContentEqual(cand.source, cand.frozen) {
		if a.host.RolloutLogger() != nil && cand.source != nil {
			a.host.RolloutLogger().Warn("supervisor: the staged candidate's source config changed after it was " +
				"proposed; applying the content the cohort agreed on instead")
		}
		return cand.frozen
	}
	return cand.source
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

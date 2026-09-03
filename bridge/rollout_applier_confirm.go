package bridge

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// Confirm-window (design §8.1) applier methods, split from rollout_applier.go: the
// provisional swap, per-member convergence recording, the confirmed-artifact
// advance, and the revert-to-N-1 (observed Revert + local deadman). The applier
// struct, its step() routing, and the shared apply/gate helpers live in
// rollout_applier.go.

// confirmDeadmanGraceTicks delays a member's LOCAL confirm-window deadman revert
// past the confirm deadline by this many poll intervals, so the coordinator's
// Revert (which members follow directly) wins in the healthy case. The local
// deadman is the belt-and-braces for a DEAD coordinator (design §8.1): without a
// coordinator to write Reverted, each member still reverts on its own timer.
const confirmDeadmanGraceTicks = 3

// adoptProvisional drives the confirm-window (design §8.1) path for a
// provisionally-committed generation: swap provisionally (WITHOUT advancing the
// durable committed artifact, so a crash reboots onto the last CONFIRMED gen),
// record convergence once this member is ready, and revert locally if the confirm
// window expires without a coordinator decision (the dead-coordinator deadman).
func (a *rolloutApplier) adoptProvisional(ctx context.Context, r persistence.Rollout) error {
	gen := r.Generation()
	if a.revertedGen == gen {
		// Already reverted this gen locally; wait for the coordinator's terminal
		// decision. Confirmed re-applies N; Reverted keeps this member on N-1.
		return nil
	}
	cand, staged := a.barrier.candidate(r.ConfigDigest())
	if !staged {
		// Not this member's own live candidate — e.g. it rebooted onto the last
		// confirmed generation (N-1) and never staged N. It stays on its safe config
		// and waits for the terminal decision: the RFC 6241 "reboot reverts" rule —
		// the provisional generation never becomes the running config here until the
		// cohort confirms it.
		return nil
	}
	if a.provisionalGen != gen {
		// First observation of this provisional commit: remember the config this
		// member is running (N-1) as the revert target, then swap provisionally.
		a.revertCfg = a.host.Config()
		a.provisionalGen = gen
		a.convergeSent = false
		// Cache when the LOCAL deadman fires, so it runs off this member's own
		// state on the drive's cadence rather than behind a store read. Delayed a
		// few ticks past the confirm deadline so a live coordinator's Revert wins;
		// the local deadman covers a dead one — or a store no decision can reach.
		a.provisionalDeadline = r.ConfirmDeadline().
			Add(time.Duration(confirmDeadmanGraceTicks) * a.barrier.pollInterval)
	}
	// Swap provisionally, idempotent once applied. A deterministically-failing swap
	// paces itself past the bound (applyCommittedGeneration), so this does not
	// rebuild the runtime every poll; the deadman below still runs, so a stuck
	// member on a dead-coordinator cohort still reverts to N-1.
	swapped := configContentEqual(a.host.Config(), cand.frozen)
	if !swapped {
		swapped = a.applyCommittedGeneration(ctx, gen, r.ConfigVersion(), a.adoptable(cand), cand.frozen)
	}
	if swapped {
		// Convergence is recorded ONLY when this member actually runs the candidate,
		// so a member still on N-1 (swap not yet taken, or given up) never counts
		// toward the confirm barrier.
		if err := a.recordConvergence(ctx, r); err != nil {
			return err
		}
	}
	// The deadman also runs from the drive tick (rollout_recovery.go), off the
	// cached deadline, so it fires on ticks where this observation never happened
	// because the store did not answer. Running it here too costs nothing and
	// keeps a member that DID observe from waiting a further tick.
	a.runLocalDeadman(ctx)
	return nil
}

// recordConvergence writes this member's Converge exactly once, when its post-swap
// readiness check passes. Convergence is unretractable, so a Nack-
// like premature write must never happen: it writes only on a genuine ready signal.
func (a *rolloutApplier) recordConvergence(ctx context.Context, r persistence.Rollout) error {
	if a.convergeSent {
		return nil
	}
	if _, already := r.Converged()[a.memberID]; already {
		a.convergeSent = true
		return nil
	}
	ready, provable := a.host.Converged(ctx)
	if !ready {
		return nil // not yet ready; a later poll retries
	}
	if !provable && len(r.Converged()) == 0 {
		// Ready, but over nothing: every session this member has defers its
		// connect until it wins a lease and it has not won one, so it has not
		// spoken to a broker since the swap. Immediately after a provisional swap
		// that is EVERY member of a lease-based cohort, including the one about to
		// take the lease — and if they all record convergence here the coordinator
		// confirms a config none of them has tried, which is the one outcome the
		// window exists to prevent.
		//
		// So a member with nothing to prove waits for a member that has something
		// to prove to go first. Once any peer has recorded convergence for this
		// generation the cohort has its demonstration, and a genuine standby —
		// which can never produce one itself — may agree. If no member ever
		// produces one, nobody converges, the window expires and the cohort goes
		// back, which is the correct answer for a change nothing could verify.
		return nil
	}
	if err := a.ops().run(ctx, rolloutOpVote, func(callCtx context.Context) error {
		return a.store.Converge(callCtx, r.Generation(), a.memberID)
	}); err != nil {
		// A resolved/superseded rollout rejecting the converge is the barrier working
		// (the coordinator reverted or confirmed under us), not a store outage.
		if isRolloutVoteRejection(err) {
			return nil
		}
		return err
	}
	a.convergeSent = true
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Info("supervisor: recorded convergence for the provisional cluster rollout",
			"generation", r.Generation(), "config_version", r.ConfigVersion(), "member", a.memberID)
	}
	return nil
}

// adoptConfirmed completes a confirmed confirm-window rollout: ensure this member
// runs the confirmed generation and NOW advance the durable last-committed
// artifact, so a crash after this reboots onto the confirmed generation.
func (a *rolloutApplier) adoptConfirmed(ctx context.Context, r persistence.Rollout) error {
	gen := r.Generation()
	if a.artifactGen == gen {
		// Already applied AND advanced the durable artifact for this confirmed
		// generation; nothing more to do on subsequent polls of the terminal row.
		return nil
	}
	cand, staged := a.barrier.candidate(r.ConfigDigest())
	if !staged {
		// Not staged (member was down during the window): it converges to the
		// generation through the durable committed artifact a converged peer wrote
		// (reconcileMissedCommit, run each step). Do not record the gen here.
		return nil
	}
	// The cohort confirmed, so N is the config to run even if a local deadman had
	// reverted this member to N-1 in a prior tick — and any revert still owed for N
	// is now the WRONG outcome, so it is cancelled rather than left to fire.
	// Idempotent when this member already runs N; a swap that keeps failing paces
	// itself past the bound and reports terminal, so this cannot become a
	// rebuild-every-poll loop.
	a.cancelRevert(gen)
	a.revertedGen = 0
	// Route through applyCommittedGeneration even when the content already
	// matches — its first branch IS that check, and going through it is what
	// resolves an outstanding repair and its terminal latch. A member whose
	// REVERT of this generation failed to the bound never left it, so a confirm
	// finds it already running the config: skipping the call there would leave it
	// latched "running the REJECTED config, replace this member" for a generation
	// the cohort has since confirmed.
	if applied := a.applyCommittedGeneration(ctx, gen, r.ConfigVersion(), a.adoptable(cand), cand.frozen); applied {
		// NOW advance the durable last-committed artifact — only on Confirm, so a
		// crash reboots onto the CONFIRMED generation, never a provisional one. The
		// generation is latched only once the write is verified durable, so a
		// confirmed member that cannot record it keeps retrying instead of
		// promising a boot state it does not have.
		a.requireCommittedArtifact(ctx, gen, r.ConfigVersion(), cand.frozen)
	}
	return nil
}

// revertProvisional reverts this member to the last confirmed generation (N-1) on
// observing a Reverted rollout (the coordinator's deadman decision). A no-op for a
// member that never provisionally applied this generation (it is already on N-1) or
// already reverted it. The swap itself is a retried, verified repair
// (rollout_recovery.go): a revert that did not take is not a revert.
func (a *rolloutApplier) revertProvisional(ctx context.Context, r persistence.Rollout) {
	if a.provisionalGen != r.Generation() {
		return
	}
	a.requireRevert(ctx, r.Generation(),
		"coordinated cluster rollout reverted by the coordinator: "+r.Reason())
}

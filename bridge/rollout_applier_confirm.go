package bridge

import (
	"context"
	"fmt"
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
	}
	// Swap provisionally, idempotent once applied. Skip re-applying once this member
	// has GIVEN UP on the swap for this gen, so a deterministically-failing swap does
	// not rebuild the runtime every poll (the deadman below still runs, so a stuck
	// member on a dead-coordinator cohort still reverts to N-1).
	swapped := configContentEqual(a.host.Config(), cand.frozen)
	if !swapped && a.gaveUpGen != gen {
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
	// Local deadman (design §8.1): if the confirm window expired and no terminal
	// decision landed, revert to the last confirmed generation. Delayed a few ticks
	// past the deadline so a live coordinator's Revert wins; this covers a DEAD one.
	grace := time.Duration(confirmDeadmanGraceTicks) * a.barrier.pollInterval
	if a.clk != nil && a.clk.Now().After(r.ConfirmDeadline().Add(grace)) {
		a.applyRevert(ctx, r, "local confirm-window deadman: the window expired without a coordinator "+
			"decision, reverting to the last confirmed generation")
		a.revertedGen = gen
	}
	return nil
}

// recordConvergence writes this member's Converge exactly once, when its post-swap
// readiness check (MQTT-R1) passes. Convergence is unretractable (I6), so a Nack-
// like premature write must never happen: it writes only on a genuine ready signal.
func (a *rolloutApplier) recordConvergence(ctx context.Context, r persistence.Rollout) error {
	if a.convergeSent {
		return nil
	}
	if _, already := r.Converged()[a.memberID]; already {
		a.convergeSent = true
		return nil
	}
	if !a.host.Converged(ctx) {
		return nil // not yet ready; a later poll retries
	}
	if err := a.store.Converge(ctx, r.Generation(), a.memberID); err != nil {
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
	// reverted this member to N-1 in a prior tick. Idempotent when this member
	// already runs N; suppress rebuild churn once it has given up on the swap (that
	// member is degraded and alarmed, F8 — a confirmed gen it cannot apply is a
	// per-node convergence failure, not a retry-forever).
	a.revertedGen = 0
	applied := configContentEqual(a.host.Config(), cand.frozen)
	if !applied && a.gaveUpGen != gen {
		applied = a.applyCommittedGeneration(ctx, gen, r.ConfigVersion(), a.adoptable(cand), cand.frozen)
	}
	if applied {
		// NOW advance the durable last-committed artifact — only on Confirm, so a
		// crash reboots onto the CONFIRMED generation, never a provisional one.
		a.persistCommittedArtifact(ctx, r, cand.frozen)
		a.artifactGen = gen
	}
	return nil
}

// revertProvisional reverts this member to the last confirmed generation (N-1) on
// observing a Reverted rollout (the coordinator's deadman decision). A no-op for a
// member that never provisionally applied this generation (it is already on N-1) or
// already reverted it.
func (a *rolloutApplier) revertProvisional(ctx context.Context, r persistence.Rollout) {
	gen := r.Generation()
	if a.provisionalGen != gen || a.revertedGen == gen || a.revertCfg == nil {
		return
	}
	a.applyRevert(ctx, r, "coordinated cluster rollout reverted by the coordinator: "+r.Reason())
	a.revertedGen = gen
}

// applyRevert swaps this member back to its cached revert target (the config it ran
// before the provisional swap, i.e. generation N-1). Idempotent: a no-op when this
// member already runs the target. A revert to a config this member ran moments ago
// should not fail; if it somehow does, the member is marked degraded rather than
// left silently mixed-version.
func (a *rolloutApplier) applyRevert(ctx context.Context, r persistence.Rollout, reason string) {
	if a.revertCfg == nil || configContentEqual(a.host.Config(), a.revertCfg) {
		return
	}
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Warn("supervisor: reverting the provisional cluster rollout to the last "+
			"confirmed generation", "generation", r.Generation(), "reason", reason)
	}
	a.host.ApplyCommitted(ctx, a.revertCfg)
	if !configContentEqual(a.host.Config(), a.revertCfg) {
		a.host.MarkDegraded(fmt.Sprintf("coordinated cluster rollout generation %d reverted, but this member "+
			"could not re-apply the last confirmed generation; it may run the reverted provisional config",
			r.Generation()))
	}
}

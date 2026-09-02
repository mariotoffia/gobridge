package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The post-commit half of the coordinated cluster rollout: getting THIS member
// to the generation the cohort decided on, and keeping the window in which it
// has not got there bounded and visible (ADR 0013). Split from
// rollout_applier.go, which owns the observe/vote/adopt routing that calls in
// here, and from rollout_recovery.go, which owns the two other local repairs
// (the durable artifact and the confirm-window revert) and the rolloutRepair
// pacing all three share.

// maxAdoptAttempts is how many attempts a member makes at a decided generation
// before the retry stops running at the drive's cadence and backs off — and
// declares itself terminal. It bounds the RATE, not the total: retrying a
// deterministic failure every poll interval rebuilds the runtime forever, which
// is an outage rather than a recovery, but a member that stopped trying
// altogether would be a permanently split cohort.
const maxAdoptAttempts = 3

// applyCommittedGeneration swaps the runtime to a generation the cohort has
// decided on, and owns the retry/terminal behaviour shared by adopt (a staged
// candidate) and reconcileMissedCommit (config decoded from the durable
// artifact). apply is the pointer handed to applyBarrierCommitted (adopt passes
// the source pointer so the manager correlates the swap; reconcile has only the
// decoded config); content is the agreed content the swap is verified against. It
// returns true once this member is running that content.
//
// The barrier is atomic BEFORE the commit and per-member after it (ADR 0013), so
// this is where the cohort's one convergence window lives. It must be both
// BOUNDED and UNENDING, which is less contradictory than it sounds:
//
//   - unending, because a member that stops trying is a permanently split cohort.
//     Most causes here (a broker refusing one connection, a store briefly
//     unopenable) outlast three fast attempts and then clear on their own; a
//     member that gave up would sit on the previous generation until an operator
//     noticed, which is exactly the outcome the barrier exists to prevent.
//   - bounded, because rebuilding the runtime every poll interval against a
//     DETERMINISTIC failure is a self-inflicted outage. So past the bound the
//     retry keeps going at the capped backoff and the member declares itself
//     terminal: it cannot reach the cohort's decision on its own, and replacing it
//     is the repair (a replacement boots on the committed artifact).
func (a *rolloutApplier) applyCommittedGeneration(ctx context.Context, gen uint64, version int, apply, content *ports.BridgeConfig) bool {
	// Restart-safe idempotence without a durable counter: a node that already runs
	// this generation's content must not rebuild its runtime just because its
	// in-memory gate was reset by the restart.
	if configContentEqual(a.host.Config(), content) {
		a.gate.record(gen)
		a.finishApply(gen)
		return true
	}
	if a.applyRepair == nil || a.applyRepair.gen != gen {
		a.applyRepair = &rolloutRepair{gen: gen, version: version}
	}
	p := a.applyRepair
	now := a.now()
	if !p.due(now) {
		return false // past the bound, waiting out the capped backoff
	}
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Info("supervisor: coordinated cluster rollout committed; applying the candidate",
			"generation", gen, "config_version", version)
	}
	p.attempts++
	a.attemptedGen = gen
	a.host.ApplyCommitted(ctx, apply)

	// Did the swap actually take? applyConfig publishes the new config only on a
	// path that adopted it (including the admin-paused branch, which records it
	// for resume) and restores the OLD config when the swap fails. Comparing
	// content is therefore the honest answer, and it matters: the barrier has
	// already decided cluster-wide, so a member that silently fails to apply is
	// running an older generation than its peers.
	if configContentEqual(a.host.Config(), content) {
		a.gate.record(gen)
		a.finishApply(gen)
		return true
	}
	// The swap failed. It recovered the old config, counted a reload failure, and
	// latched degraded — but on its own that leaves this member on the previous
	// generation while its peers moved on.
	if p.attempts >= maxAdoptAttempts {
		a.markUnsafe(gen, rolloutRepairApply, fmt.Sprintf(
			"coordinated cluster rollout generation %d (config version %d) was decided by the cohort but "+
				"could not be applied on this member after %d attempts; IT RUNS AN OLDER CONFIG "+
				"GENERATION THAN THE REST OF THE COHORT. It keeps retrying at a capped backoff in case "+
				"the cause clears — if it does not, replace this member: a replacement boots on the "+
				"committed artifact", gen, version, p.attempts))
		p.deferNext(now, a.pollInterval())
	} else if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Error("supervisor: applying the decided cluster rollout generation failed; retrying "+
			"(this member runs an OLDER generation than the cohort until it succeeds)",
			"generation", gen, "attempt", p.attempts, "max_attempts", maxAdoptAttempts)
	}
	a.obs.observeRetry(rolloutRepairApply, p.attempts)
	return false
}

// finishApply clears the apply repair once this member is verifiably running the
// generation, and offers the completion to the terminal latch — which retracts
// only if an apply is what can end it (see unsafeResolvedBy).
func (a *rolloutApplier) finishApply(gen uint64) {
	if a.applyRepair == nil && a.terminalGen == 0 {
		return
	}
	a.applyRepair = nil
	a.obs.observeRetry(rolloutRepairApply, 0)
	a.clearUnsafe(rolloutRepairApply, gen)
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

// reconcileMissedCommit converges a running member to the durable last-committed
// artifact when it is AHEAD of the generation this member has applied — the case
// where the member missed a commit because the active rollout row was overwritten
// by the next proposal before it observed the commit (design residual seq (2)),
// or was down when the commit happened and never staged the candidate. It fetches
// the committed BYTES (option (a)), so it needs no staged candidate.
//
// A no-op when no codec is wired, the artifact is not ahead, or the decoded bytes
// fail their digest check (the running config is then kept — better than building
// a corrupt artifact). Returns a store error to the caller for retry.
//
// Wiring obligation, and it is LIVE: reconcile (like the joiner's boot
// substitution) applies a config the config MANAGER did not emit — the decoded
// artifact, not a manager-correlated pointer. A composition root that wires
// onSwap -> a ports config manager must re-sync the manager's desired/running
// fingerprint after a barrier-driven swap, or ReconfigurePending and deep-health
// Degraded latch true despite the member being correctly converged.
//
// The shipped AWS root does drive this barrier (bootstrap.App hosts the same
// applier through ports.RolloutHost), and it discharges the obligation by calling
// the manager's AdoptRunning after a barrier swap. A new root that hosts the
// barrier owes the same step, plus the deployment-profile fingerprint admission
// the applier gate expects.
func (a *rolloutApplier) reconcileMissedCommit(ctx context.Context, decidedGen uint64) error {
	if a.barrier.decode == nil || a.barrier.committedStore == nil {
		return nil
	}
	committed, err := rolloutOpValue(ctx, a.ops(), rolloutOpRead, a.barrier.committedStore.CommittedConfig)
	if errors.Is(err, shared.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !a.gate.admits(committed.Generation) {
		return nil // already applied this generation (or newer), or the baseline seed
	}
	if committed.Generation < decidedGen {
		// The cohort has since DECIDED a newer generation, so the durable artifact
		// is a superseded one and the active-row path owns convergence. Applying it
		// here would not merely be pointless: the two paths key their bounded retry
		// on the generation, so alternating between them resets each other's backoff
		// and rebuilds the runtime every poll — the outage the bound exists to
		// prevent.
		return nil
	}
	if a.attemptedGen >= committed.Generation {
		// adopt already ATTEMPTED this generation (or newer) through the
		// staged-candidate path in this same step, including a transient failure that
		// left the gate unrecorded. Re-driving it here would consume a second retry
		// attempt in the same poll; leave it to adopt's next-poll retry. An adopt that
		// merely returned early — no staged candidate — attempted nothing, and this is
		// the path that then converges the member from the durable artifact.
		return nil
	}
	cfg, err := a.barrier.decode(committed.ConfigBytes)
	if err != nil {
		if a.host.RolloutLogger() != nil {
			a.host.RolloutLogger().Error("supervisor: the durable last-committed rollout artifact could not be "+
				"decoded to reconcile a missed commit; the running config is kept",
				"generation", committed.Generation, "error", err)
		}
		return nil
	}
	// Integrity: the reconstructed config must match the digest the artifact
	// records, or the bytes are corrupt and must not be built.
	if raw, ok := configCanonicalBytes(cfg); !ok || candidateConfigDigest(raw) != committed.Digest {
		if a.host.RolloutLogger() != nil {
			a.host.RolloutLogger().Error("supervisor: the durable last-committed rollout artifact failed its digest "+
				"check on reconcile; the running config is kept", "generation", committed.Generation)
		}
		return nil
	}
	a.applyCommittedGeneration(ctx, committed.Generation, committed.ConfigVersion, cfg, cfg)
	return nil
}

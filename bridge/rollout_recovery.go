package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Local safety-state repair for the coordinated cluster rollout applier.
//
// Once the cohort has decided, two operations decide whether THIS member is
// safe, and neither is finished when its call returns:
//
//   - recording the durable last-committed artifact. If it never lands, this
//     member reboots onto a generation older than the one the cohort runs.
//   - reverting a provisional generation to the last confirmed one. If it never
//     lands, this member keeps serving a config the cohort rejected.
//
// Both were one-shot and best-effort, and both LATCHED as done regardless of
// outcome — so a single cancelled write or a single failed swap left the member
// permanently wrong with its own repair path switched off. Here they are modelled
// as retryable state instead: attempted under a bounded backoff, latched only
// after the result is VERIFIED (read back for the artifact, compared against the
// running config for the revert), and — when the bound is reached — declared
// terminal, because a member that cannot reach its safe generation cannot fix
// itself and must be replaced.

const (
	// maxRolloutRepairAttempts bounds each local repair. The common causes are
	// transient (a throttled store, a broker refusing one reconnect), so retrying
	// genuinely converges; but an unbounded retry of a deterministic failure is an
	// outage that hides itself, so the bound is what turns it into an alarm.
	maxRolloutRepairAttempts = 5
	// maxRolloutRepairBackoff caps the exponential backoff between attempts.
	maxRolloutRepairBackoff = 30 * time.Second

	// Repair classes, used as the operation dimension on the retry gauge and in
	// the terminal reason an operator reads. "apply" is the bounded swap retry in
	// applyCommittedGeneration; it is not driven from this file but reports on the
	// same gauge, because an operator asking "what is this member retrying" wants
	// one series, not three.
	rolloutRepairApply    = "apply"
	rolloutRepairArtifact = "artifact"
	rolloutRepairRevert   = "revert"
)

// rolloutRepair is one outstanding local safety operation: which generation it
// belongs to, what to write or apply, how many attempts it has taken, and when
// the next one is due.
type rolloutRepair struct {
	gen      uint64
	version  int
	cfg      *ports.BridgeConfig
	reason   string // revert only: why this member is going back
	attempts int
	nextAt   time.Time
}

// due reports whether the next attempt may run. A repair that has never been
// attempted is due immediately — the first attempt is not a retry.
func (p *rolloutRepair) due(now time.Time) bool {
	return p.nextAt.IsZero() || !now.Before(p.nextAt)
}

// deferNext schedules the next attempt one exponential step out from base,
// capped. Backoff matters here because both repairs talk to something that is
// already struggling: hammering a throttled store or rebuilding a runtime every
// poll interval is how a recoverable failure becomes an unrecoverable one.
func (p *rolloutRepair) deferNext(now time.Time, base time.Duration) {
	step := orDefault(base, defaultRolloutPollInterval)
	for i := 1; i < p.attempts; i++ {
		if step >= maxRolloutRepairBackoff {
			break
		}
		step *= 2
	}
	if step > maxRolloutRepairBackoff {
		step = maxRolloutRepairBackoff
	}
	p.nextAt = now.Add(step)
}

// ops returns the barrier's bounded-call helper, or nil for a hand-wired applier
// with no barrier behind it (which then runs its calls unbounded).
func (a *rolloutApplier) ops() *rolloutOps {
	if a.barrier == nil {
		return nil
	}
	return a.barrier.ops
}

// now reads the applier's clock, defaulting to the system clock.
func (a *rolloutApplier) now() time.Time {
	if a.clk == nil {
		return clock.System.Now()
	}
	return a.clk.Now()
}

// pollInterval is the barrier's cadence, and the base of every repair backoff.
func (a *rolloutApplier) pollInterval() time.Duration {
	if a.barrier == nil {
		return defaultRolloutPollInterval
	}
	return a.barrier.pollInterval
}

// tick is one drive iteration. The ORDER is the whole point of this function:
// the two local operations run BEFORE the store observation, off state this
// member already holds. A store that has stopped answering therefore delays them
// by at most one tick of bounded calls — and only the FIRST such tick, because
// once a call has been abandoned the barrier refuses to start another until it
// returns, so every later tick is instant. It cannot suppress them. That matters
// because a rollout store outage and a dead coordinator are the same outage from
// a member's point of view, and the local deadman is the only thing that gets it
// back to a confirmed config in either.
func (a *rolloutApplier) tick(ctx context.Context) error {
	a.runLocalDeadman(ctx)
	a.driveRevert(ctx)
	err := a.step(ctx)
	a.driveArtifact(ctx)
	a.obs.publishFreshness()
	return err
}

// runLocalDeadman fires this member's confirm-window deadman (design §8.1) from
// the CACHED provisional deadline, so it needs no store read. Delayed a few ticks
// past the deadline so a live coordinator's Revert wins; this covers a dead one —
// or a store that no decision can be written to.
func (a *rolloutApplier) runLocalDeadman(ctx context.Context) {
	gen := a.provisionalGen
	if gen == 0 || a.revertedGen == gen || a.provisionalDeadline.IsZero() {
		return
	}
	if a.now().Before(a.provisionalDeadline) {
		return
	}
	a.requireRevert(ctx, gen, "local confirm-window deadman: the window expired without a coordinator "+
		"decision, reverting to the last confirmed generation")
}

// requireRevert records that this member owes a revert to its last confirmed
// generation and attempts it immediately. A member that never provisionally
// applied the generation owes nothing: it is already on the safe config.
func (a *rolloutApplier) requireRevert(ctx context.Context, gen uint64, reason string) {
	if a.revertCfg == nil || a.revertedGen == gen {
		return
	}
	if a.terminalGen == gen {
		// This member already exhausted its attempts at this generation's safe
		// state. Re-arming on the next observation of the same Reverted row would
		// turn the bound into a rebuild-every-poll loop, which is the outage the
		// bound exists to prevent.
		return
	}
	if a.pendingRevert == nil || a.pendingRevert.gen != gen {
		a.pendingRevert = &rolloutRepair{gen: gen, cfg: a.revertCfg, reason: reason}
	}
	a.driveRevert(ctx)
}

// cancelRevert drops an outstanding revert for gen. The cohort CONFIRMED that
// generation, so going back to N-1 is no longer the safe outcome — it is the
// wrong one.
func (a *rolloutApplier) cancelRevert(gen uint64) {
	if a.pendingRevert != nil && a.pendingRevert.gen == gen {
		a.pendingRevert = nil
		a.obs.observeRetry(rolloutRepairRevert, 0)
	}
	a.provisionalDeadline = time.Time{}
}

// driveRevert makes one bounded attempt at an outstanding revert. The generation
// is marked reverted only once this member is VERIFIABLY running the last
// confirmed config: a revert that did not take is not a revert, and latching it
// would leave the member on the rejected provisional config with its own repair
// switched off.
func (a *rolloutApplier) driveRevert(ctx context.Context) {
	p := a.pendingRevert
	if p == nil {
		return
	}
	if p.gen < a.gate.applied {
		// Superseded: this member has since applied a NEWER generation, so the
		// revert target is no longer its safe config. Left armed, the repair and
		// the newer generation's adopt would swap the runtime back and forth on
		// every poll — the revert applying N-1, the adopt re-applying N+1.
		a.pendingRevert = nil
		a.obs.observeRetry(rolloutRepairRevert, 0)
		return
	}
	if configContentEqual(a.host.Config(), p.cfg) {
		a.finishRevert()
		return
	}
	now := a.now()
	if !p.due(now) {
		return
	}
	p.attempts++
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Warn("supervisor: reverting the provisional cluster rollout to the last "+
			"confirmed generation", "generation", p.gen, "attempt", p.attempts, "reason", p.reason)
	}
	a.host.ApplyCommitted(ctx, p.cfg)
	if configContentEqual(a.host.Config(), p.cfg) {
		a.finishRevert()
		return
	}
	if p.attempts >= maxRolloutRepairAttempts {
		a.pendingRevert = nil
		a.markUnsafe(p.gen, rolloutRepairRevert, fmt.Sprintf(
			"coordinated cluster rollout generation %d was reverted, but this member could not re-apply "+
				"the last confirmed generation after %d attempts; it is still running the REJECTED "+
				"provisional config and cannot repair itself — replace this member",
			p.gen, p.attempts))
		return
	}
	p.deferNext(now, a.pollInterval())
	a.obs.observeRetry(rolloutRepairRevert, p.attempts)
}

// finishRevert latches a verified revert.
func (a *rolloutApplier) finishRevert() {
	gen := a.pendingRevert.gen
	a.revertedGen = gen
	a.pendingRevert = nil
	a.obs.observeRetry(rolloutRepairRevert, 0)
	a.clearUnsafe(gen)
}

// requireCommittedArtifact records that this member owes the durable
// last-committed artifact for gen, and attempts it immediately. A no-op when no
// codec is wired (the residual protection is disabled) or when the artifact for
// this generation is already verified durable.
func (a *rolloutApplier) requireCommittedArtifact(
	ctx context.Context, gen uint64, version int, cfg *ports.BridgeConfig,
) {
	if a.barrier == nil || a.barrier.encode == nil || a.barrier.committedStore == nil {
		return
	}
	if a.artifactGen >= gen || a.terminalGen == gen {
		return
	}
	if a.pendingArtifact == nil || a.pendingArtifact.gen != gen {
		a.pendingArtifact = &rolloutRepair{gen: gen, version: version, cfg: cfg}
	}
	a.driveArtifact(ctx)
}

// driveArtifact makes one bounded attempt at the outstanding artifact write and
// VERIFIES it by reading the artifact back. The read-back is not paranoia: the
// port's monotonicity rule makes a write for an older generation a no-op
// SUCCESS, so "the write returned nil" and "the artifact holds this generation"
// are genuinely different statements, and only the second one means this member
// can boot on what the cohort committed.
func (a *rolloutApplier) driveArtifact(ctx context.Context) {
	p := a.pendingArtifact
	if p == nil {
		return
	}
	now := a.now()
	if !p.due(now) {
		return
	}
	p.attempts++
	err := a.barrier.writeCommittedArtifact(ctx, p.gen, p.version, p.cfg)
	if err == nil {
		err = a.verifyCommittedArtifact(ctx, p.gen)
	}
	if err == nil {
		a.artifactGen = p.gen
		a.pendingArtifact = nil
		a.obs.observeArtifact(p.gen)
		a.obs.observeRetry(rolloutRepairArtifact, 0)
		a.clearUnsafe(p.gen)
		return
	}
	// A digest mismatch at this generation is corruption, not an outage: two
	// different configs cannot share one committed generation, and no retry
	// resolves that. Give up on the first one.
	if errors.Is(err, shared.ErrRolloutDigestMismatch) {
		a.pendingArtifact = nil
		a.markUnsafe(p.gen, rolloutRepairArtifact, fmt.Sprintf(
			"the durable last-committed rollout artifact already holds a DIFFERENT config at generation "+
				"%d (%v); two configs cannot share a committed generation, so this member has stopped "+
				"trying to record it and a restart would boot the wrong config — investigate the rollout "+
				"store before restarting this member", p.gen, err))
		return
	}
	// Past the bound this becomes an alarm, but NOT a give-up: the write is
	// idempotent and harmless, the member is running the right config, and the
	// only thing wrong is what it would boot on. Replacing the member is the one
	// action that would make that WORSE, so it keeps retrying at the capped
	// backoff until the store comes back.
	if p.attempts >= maxRolloutRepairAttempts {
		a.markUnsafe(p.gen, rolloutRepairArtifact, fmt.Sprintf(
			"coordinated cluster rollout generation %d is applied on this member but its durable "+
				"last-committed artifact could not be recorded and verified after %d attempts (%v); a "+
				"restart would boot an older generation than the cohort runs. This member keeps "+
				"retrying — repair the rollout store; do NOT replace the member, which would boot it on "+
				"the older generation", p.gen, p.attempts, err))
	} else if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Warn("supervisor: recording the durable last-committed rollout artifact failed; "+
			"retrying (a restart before it lands would boot an older generation than the cohort runs)",
			"generation", p.gen, "attempt", p.attempts, "error", err)
	}
	p.deferNext(now, a.pollInterval())
	a.obs.observeRetry(rolloutRepairArtifact, p.attempts)
}

// verifyCommittedArtifact reads the artifact back and reports whether gen is
// durable. A stored generation AHEAD of gen also passes: a peer advanced the
// artifact past this generation, so a boot lands on something newer, never older.
func (a *rolloutApplier) verifyCommittedArtifact(ctx context.Context, gen uint64) error {
	committed, err := rolloutOpValue(ctx, a.ops(), rolloutOpRead, a.barrier.committedStore.CommittedConfig)
	if err != nil {
		return fmt.Errorf("bridge: the durable last-committed rollout artifact could not be read back to "+
			"verify generation %d: %w", gen, err)
	}
	if committed.Generation < gen {
		return fmt.Errorf("bridge: the durable last-committed rollout artifact still reads generation %d "+
			"after writing generation %d: %w", committed.Generation, gen, shared.ErrUnavailable)
	}
	return nil
}

// markUnsafe latches a generation whose safe state this member could not reach
// on its own: it cannot boot on what the cohort committed, or it is running what
// the cohort rejected. Neither resolves without operator action, which is why it
// is louder than the bounded give-up on an APPLY (that one leaves the member on
// an older but valid generation).
//
// It says nothing about whether retrying continues — the caller decides that,
// because the right answer differs: a member that cannot RECORD the artifact is
// running the correct config and must keep trying (replacing it is what would
// boot it on the older generation), while a member that cannot REVERT is running
// rejected config and replacing it IS the repair. The reason text carries that
// difference to the operator, who is the one who acts on it.
//
// Idempotent per generation: the artifact path calls it on every further failed
// attempt.
func (a *rolloutApplier) markUnsafe(gen uint64, class, reason string) {
	if a.terminalGen == gen {
		return
	}
	a.terminalGen = gen
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Error("supervisor: THIS MEMBER CANNOT REACH THE SAFE STATE OF A COORDINATED "+
			"CLUSTER ROLLOUT GENERATION on its own and needs operator attention",
			"generation", gen, "operation", class, "reason", reason)
	}
	a.host.MarkDegraded(reason)
	a.obs.observeTerminal(gen, reason)
}

// clearUnsafe retracts the latch once a repair for gen actually completes — the
// store came back and the artifact landed, or the revert finally took. A latch
// for an EARLIER generation is cleared too: an artifact that now holds
// generation N answers for every generation below it, because that is what this
// member would boot on.
//
// It is called only from a repair that VERIFIABLY completed, never from a
// successful apply: a member running the committed config with its artifact
// still unwritten is exactly the unsafe state the latch describes.
//
// The host's own degraded latch is not retracted here (RolloutHost has no
// un-degrade, and it clears on the next successful reload); the rollout status
// and its gauge are, because they describe a condition that is genuinely over.
func (a *rolloutApplier) clearUnsafe(gen uint64) {
	if a.terminalGen == 0 || a.terminalGen > gen {
		return
	}
	a.terminalGen = 0
	a.obs.observeTerminal(0, "")
	if a.host.RolloutLogger() != nil {
		a.host.RolloutLogger().Info("supervisor: the coordinated cluster rollout state this member could "+
			"not safely reach has now been reached; the member is consistent with the cohort again",
			"generation", gen)
	}
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
// wiring dependency: reconcile (like the joiner's boot substitution)
// applies a config the config MANAGER did not emit — the decoded artifact, not a
// manager-correlated pointer. A production composition root that wires
// onSwap -> ports config manager NotifyApplyResult must reconcile the manager's
// desired/running fingerprint after a barrier-driven swap (e.g. re-sync running to
// the committed config), or ReconfigurePending / deep-health Degraded can latch
// true despite the member being correctly converged. The barrier is not wired into
// any production root today, so this does not manifest here. Wiring the barrier
// into a production root is what makes this reconcile step mandatory.
func (a *rolloutApplier) reconcileMissedCommit(ctx context.Context, adoptGen uint64) error {
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
	if adoptGen >= committed.Generation {
		// adopt already drove this generation (or newer) through the staged-candidate
		// path in this same step — including a transient failure that left the gate
		// unrecorded. Re-driving it here would consume a second retry attempt in the
		// same poll; leave it to adopt's next-poll retry.
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

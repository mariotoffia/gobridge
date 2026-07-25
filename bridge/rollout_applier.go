package bridge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

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
//     (F10), so a divergent config aborts the rollout instead of splitting the
//     cohort.
//
// So each member STAGES the candidate its own source delivered
// (rolloutBarrier.stage) and the applier builds from that. The safety hole §5
// named for option (b) — `current` is written before the barrier decides, so a
// member restarting after an ABORTED rollout would boot the rejected config
// while the cohort keeps the old one (a mixed-version cohort, which G2 forbids)
// — is closed by the joiner rule in rollout_joiner.go, NOT left open.
//
// A consequence worth stating: there are no FETCHED candidate bytes, so the
// design's "verify the digest before decoding hostile input" obligation is met
// by construction. The bytes were decoded by this node's own config-source load
// path (the same validated path a single-node reload uses) before any rollout
// row was consulted; the digest check is a cross-member AGREEMENT check, not a
// trust boundary.

// evaluateProposal runs the node's pre-build applier gate for an observed
// rollout (design §6). It verifies the candidate against the recorded digest
// FIRST (F10), then classifies the delta (§8), returning an empty string when
// the node may build and Ack or a non-empty Nack reason otherwise. The build
// (and the Ack carrying its build digest) is the caller's job, performed only
// when the reason is empty.
func evaluateProposal(oldCfg, candidateCfg *ports.BridgeConfig, candidateBytes []byte, expectedDigest string) string {
	if err := verifyCandidateDigest(candidateBytes, expectedDigest); err != nil {
		return err.Error()
	}
	if class, reason := classifyRolloutDelta(oldCfg, candidateCfg); class == rolloutReplacementRequired {
		return reason
	}
	return ""
}

// nodeRolloutGate is a node's local high-water mark over rollout generations
// (research §3: "each node is itself a token-checking resource"). Generations
// are globally monotonic — there is one active rollout at a time — so a single
// generation counter is a total order and subsumes the coordinator fencing epoch
// (already enforced at the store, I3).
//
// It is deliberately IN-MEMORY ONLY. A durable high-water was considered (design
// §11 Phase 4) and is not needed: across a restart the same guarantee is
// reconstructed from state that is already durable and already authoritative —
//
//   - the store admits exactly one active rollout and hands back only the
//     newest generation through Current, so there is no channel by which a
//     STALE generation could reach a restarted node at all;
//   - Ack/Commit against a non-current generation is rejected by the store
//     (ErrNotFound), so a late vote cannot land either;
//   - re-adoption of an ALREADY-applied generation is caught by content, not by
//     a counter: rolloutApplier.adopt compares the committed candidate against
//     the running config and records the generation without swapping when they
//     match (see there).
//
// A durable counter would therefore add a per-node write path and its failure
// modes while removing no reachable failure. The in-memory mark still does real
// work WITHIN a process lifetime: it makes repeated observations of the same
// committed generation idempotent without re-deriving content equality.
type nodeRolloutGate struct {
	applied uint64 // highest fully-applied generation; 0 = none yet
}

// admits reports whether gen is strictly newer than the highest generation this
// node has applied. A stale generation (a deposed coordinator's late push) and
// an already-applied generation (a re-fired notification) are both rejected, so
// application is idempotent and never rewinds.
func (g *nodeRolloutGate) admits(gen uint64) bool {
	return gen > g.applied
}

// record advances the high-water mark to gen once the node has fully applied it.
// It is monotonic: a stale generation never lowers the mark.
func (g *nodeRolloutGate) record(gen uint64) {
	if gen > g.applied {
		g.applied = gen
	}
}

// candidateConfigDigest is the canonical digest of a candidate config artifact:
// the hex-encoded SHA-256 of its exact bytes. The proposer stamps a rollout row
// with this digest; every applier recomputes it over the candidate its own
// config source delivered and compares (F10).
//
// Determinism across members (design §11 Phase 4 "UNPROVEN") holds because the
// digest input, configCanonicalBytes, is a pure function of the config document:
// it JSON-encodes shared.RevealSecrets(cfg), and a shared.Secret is a literal
// value carried in the config bytes — GoBridge performs NO per-node
// interpolation (no env expansion, no lazy secret resolution) on the config load
// path, so two members loading the same document canonicalise identically. The
// remaining input is the plugin registry, which decodes the typed plugin
// payloads; a cohort whose members register different plugin sets would diverge
// here, and does so LOUDLY (a Nack naming the digest mismatch) rather than
// silently. TestCandidateConfigDigest_IsStableAcrossIndependentLoads pins the
// property; UC-CR7 (Phase 5) proves it across real processes.
func candidateConfigDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// verifyCandidateDigest checks a candidate against the digest recorded in the
// rollout row (invariant behind F10). A mismatch — a divergent config source, a
// superseded artifact — or an empty expected digest is an error: the node must
// Nack rather than build a candidate the cohort did not agree on. The compare is
// constant-time; the inputs are not secret, but it costs nothing and avoids a
// length/early-exit oracle.
func verifyCandidateDigest(raw []byte, expected string) error {
	if expected == "" {
		return shared.ErrRolloutDigestMismatch.WithMessage("rollout row carries no candidate digest to verify against")
	}
	got := candidateConfigDigest(raw)
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return shared.ErrRolloutDigestMismatch.
			WithMessage("candidate config digest mismatch").
			With("expected", expected).With("got", got)
	}
	return nil
}

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
	sup      *Supervisor
	store    ports.ClusterRolloutStore
	memberID string
	gate     nodeRolloutGate
	obs      *rolloutObserver

	// attemptGen/attempts count consecutive apply attempts for one committed
	// generation. Both are touched only from the drive goroutine.
	attemptGen uint64
	attempts   int
}

// step performs one observation of the rollout row and acts on it. Store
// notifications are hints, never truth: every decision re-reads the row through
// the store, whose implementations read consistently (research rule 11).
//
// A returned error is a STORE error only — the loop logs and retries it (F9: an
// outage flips no state, and members keep serving the old config). Every
// protocol outcome (Nack, no candidate yet, not our cohort) is a normal return.
func (a *rolloutApplier) step(ctx context.Context) error {
	r, err := a.store.Current(ctx)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			// No active rollout, but the durable artifact may still be ahead of this
			// member (a commit it missed whose row was later cleared).
			return a.reconcileMissedCommit(ctx, 0)
		}
		return err
	}
	// Publish BEFORE acting, so the observation an operator reads is what this
	// member saw and reacted to — including the "staged" flag that identifies a
	// member whose own config source has not delivered the candidate, which is
	// otherwise invisible and is the most common reason a rollout hangs.
	if a.obs != nil {
		cand, staged := a.sup.rollout.candidate(r.ConfigDigest())
		// "applied" is answered by CONTENT, not by the local gate: after a
		// restart the gate is empty even though this member is running the
		// generation, and after a failed swap the gate may be set even though it
		// is not. Content is the only answer an operator can act on.
		applied := staged && configContentEqual(a.sup.Config(), cand.frozen)
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
		if err := a.adopt(ctx, r); err != nil {
			return err
		}
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
// verdict exactly once (I5).
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
// F8 alarms rather than rolls back), and it does not prove the runtime's
// route-graph validation, which also runs in the commit phase.
//
// That last one makes the validator load-bearing for the barrier, not merely for
// config hygiene: WITHOUT one, Builder.Plan trusts the input (see its doc), so a
// dangling reference is acked by every member, committed, and then fails every
// member's swap. The cohort stays consistent — all members fail identically and
// recover the old config, so G2 holds — but the change costs a cohort-wide
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
	cand, staged := a.sup.rollout.candidate(r.ConfigDigest())
	if !staged {
		// This node's own config source has not delivered the candidate yet.
		// Staying silent is correct: the coordinator's deadline (F1) bounds the
		// wait and aborts if it never arrives, and a member that guessed instead
		// would break the agreement the digest exists to prove.
		return nil
	}
	raw, ok := configCanonicalBytes(cand.frozen)
	if !ok {
		return a.nack(ctx, r, "candidate config could not be canonicalised for digest verification")
	}
	if reason := evaluateProposal(a.sup.Config(), cand.frozen, raw, r.ConfigDigest()); reason != "" {
		return a.nack(ctx, r, reason)
	}
	plan, err := a.sup.newBuilder(cand.frozen).Plan(ctx)
	if err != nil {
		// A Nack is a PERMANENT, unretryable verdict: the aggregate admits one
		// vote per member per generation (I5) and the coordinator aborts on the
		// first one (F2). It must therefore mean "this config is wrong", never
		// "I was briefly unable to check". Plan opens stores and resolves
		// credentials, so a throttled store, a flaky credential provider, or —
		// because ctx is the drive loop's — an ordinary SIGTERM would otherwise
		// let one restarting member poison every in-flight rollout in the cohort.
		// Abstain instead and let the deadline (F1) decide; that outcome is
		// recoverable by re-proposing, a nack is not.
		if transientBuildFailure(ctx, err) {
			if a.sup.logger != nil {
				a.sup.logger.Warn("supervisor: abstaining from the cluster rollout vote; the candidate "+
					"build failed for a transient reason and a Nack would permanently abort a "+
					"recoverable rollout", "generation", r.Generation(), "error", err)
			}
			return nil
		}
		return a.nack(ctx, r, "candidate build failed on this member: "+err.Error())
	}
	plan.Close()

	// The build digest proves WHAT was built. Every member builds from the
	// digest-verified candidate, so it equals the candidate digest by
	// construction; recording it keeps the ack self-describing in the audit
	// trail and satisfies the aggregate's non-empty requirement.
	if err := a.store.Ack(ctx, r.Generation(), a.memberID, r.ConfigDigest()); err != nil {
		// A rollout that resolved (or was superseded) between the read and the
		// vote rejects it. That is the barrier working, not a store outage:
		// swallow it so the loop does not log an outage that did not happen.
		if isRolloutVoteRejection(err) {
			return nil
		}
		return err
	}
	if a.sup.logger != nil {
		a.sup.logger.Info("supervisor: acked coordinated cluster rollout candidate",
			"generation", r.Generation(), "config_version", r.ConfigVersion(), "member", a.memberID)
	}
	return nil
}

// nack records this node's rejection with an operator-facing reason. The rollout
// does NOT terminate here — the coordinator observes the nack and aborts (F2),
// so the decision stays with the single fenced decider.
func (a *rolloutApplier) nack(ctx context.Context, r persistence.Rollout, reason string) error {
	if a.sup.logger != nil {
		a.sup.logger.Error("supervisor: nacking coordinated cluster rollout candidate; the "+
			"coordinator aborts the rollout and every member keeps its running config",
			"generation", r.Generation(), "config_version", r.ConfigVersion(), "reason", reason)
	}
	if err := a.store.Nack(ctx, r.Generation(), a.memberID, reason); err != nil {
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
	cand, staged := a.sup.rollout.candidate(r.ConfigDigest())
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
		a.persistCommittedArtifact(ctx, r, cand.frozen)
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
	if configContentEqual(a.sup.Config(), content) {
		a.gate.record(gen)
		return true
	}
	if a.sup.logger != nil {
		a.sup.logger.Info("supervisor: coordinated cluster rollout committed; applying the candidate",
			"generation", gen, "config_version", version)
	}
	if a.attemptGen != gen {
		a.attemptGen, a.attempts = gen, 0
	}
	a.attempts++
	a.sup.applyBarrierCommitted(ctx, apply)

	// Did the swap actually take? applyConfig publishes the new config only on a
	// path that adopted it (including the admin-paused branch, which records it
	// for resume) and restores the OLD config when the swap fails. Comparing
	// content is therefore the honest answer, and it matters: the barrier has
	// already committed cluster-wide, so a member that silently fails to apply
	// leaves the mixed-version cohort G2 forbids.
	if configContentEqual(a.sup.Config(), content) {
		a.gate.record(gen)
		return true
	}
	// The swap failed. It recovered the old config, counted a reload failure, and
	// latched degraded — but on its own that leaves this member on the previous
	// generation while its peers moved on. Retry a bounded number of times: the
	// common causes (a broker refusing a connection, a store briefly unopenable)
	// are transient, and a retry genuinely converges the cohort.
	if a.attempts < maxAdoptAttempts {
		if a.sup.logger != nil {
			a.sup.logger.Error("supervisor: applying the committed cluster rollout failed; retrying "+
				"(this member runs an OLDER generation than the cohort until it succeeds)",
				"generation", gen, "attempt", a.attempts, "max_attempts", maxAdoptAttempts)
		}
		return false
	}
	// Out of retries: stop rebuilding the runtime in a loop and leave the
	// divergence loudly visible instead. RolloutStatus reports applied=false and
	// the supervisor stays degraded, so an operator sees WHICH member diverged
	// rather than a cohort that merely looks committed everywhere.
	a.gate.record(gen)
	if a.sup.logger != nil {
		a.sup.logger.Error("supervisor: giving up applying the committed cluster rollout after repeated "+
			"failures; THIS MEMBER RUNS AN OLDER CONFIG GENERATION THAN THE COHORT. Investigate this "+
			"node and restart it once the cause is fixed",
			"generation", gen, "config_version", version, "attempts", a.attempts)
	}
	a.sup.markDegraded(fmt.Sprintf("coordinated cluster rollout generation %d committed but could not "+
		"be applied on this member after %d attempts; it runs an older config generation than the rest "+
		"of the cohort", gen, a.attempts))
	return false
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
// PHASE-6 wiring dependency: reconcile (like the joiner's boot substitution)
// applies a config the config MANAGER did not emit — the decoded artifact, not a
// manager-correlated pointer. A production composition root that wires
// onSwap -> ports config manager NotifyApplyResult must reconcile the manager's
// desired/running fingerprint after a barrier-driven swap (e.g. re-sync running to
// the committed config), or ReconfigurePending / deep-health Degraded can latch
// true despite the member being correctly converged. The barrier is not wired into
// any production root in Phase 5, so this does not manifest here; it is a Phase-6
// obligation tracked in the design doc.
func (a *rolloutApplier) reconcileMissedCommit(ctx context.Context, adoptGen uint64) error {
	if a.sup.rollout.decode == nil || a.sup.rollout.committedStore == nil {
		return nil
	}
	committed, err := a.sup.rollout.committedStore.CommittedConfig(ctx)
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
	cfg, err := a.sup.rollout.decode(committed.ConfigBytes)
	if err != nil {
		if a.sup.logger != nil {
			a.sup.logger.Error("supervisor: the durable last-committed rollout artifact could not be "+
				"decoded to reconcile a missed commit; the running config is kept",
				"generation", committed.Generation, "error", err)
		}
		return nil
	}
	// Integrity: the reconstructed config must match the digest the artifact
	// records, or the bytes are corrupt and must not be built.
	if raw, ok := configCanonicalBytes(cfg); !ok || candidateConfigDigest(raw) != committed.Digest {
		if a.sup.logger != nil {
			a.sup.logger.Error("supervisor: the durable last-committed rollout artifact failed its digest "+
				"check on reconcile; the running config is kept", "generation", committed.Generation)
		}
		return nil
	}
	a.applyCommittedGeneration(ctx, committed.Generation, committed.ConfigVersion, cfg, cfg)
	return nil
}

// persistCommittedArtifact records the just-adopted generation as the durable
// last-committed config artifact. It is best-effort: the swap has already
// succeeded (this member runs the committed config), so a failed write must NOT
// roll it back — it only means a future restart of THIS member might fall back to
// an older committed artifact until a peer's adopt (idempotent) writes this
// generation. It is a no-op when no codec is wired.
func (a *rolloutApplier) persistCommittedArtifact(ctx context.Context, r persistence.Rollout, cfg *ports.BridgeConfig) {
	if err := a.sup.rollout.writeCommittedArtifact(ctx, r.Generation(), r.ConfigVersion(), cfg); err != nil {
		if a.sup.logger != nil {
			a.sup.logger.Warn("supervisor: recording the durable last-committed rollout artifact failed; "+
				"the swap succeeded and this member runs the committed config, but a restart before a peer "+
				"writes this generation may fall back to an older committed artifact",
				"generation", r.Generation(), "error", err)
		}
	}
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
		if a.sup.logger != nil && cand.source != nil {
			a.sup.logger.Warn("supervisor: the staged candidate's source config changed after it was " +
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

package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Coordinated cluster rollout — the proposer half of the barrier
// (design cluster-config-rollout-protocol.md §6).
//
// A clustered node refuses every live config delta by default (ADR 0012). When
// the deployment opts into cluster.rollout: coordinated AND the delta is
// live-safe (§8), the delta is instead PROPOSED to the shared rollout store as a
// candidate generation with a frozen membership epoch, and the local apply is
// DEFERRED. No member swaps until the lease-elected coordinator observes acks
// covering the epoch and flips the rollout to Committed — that store-atomic flip
// is what keeps the cohort from running mixed versions.
//
// Every member proposes the delta it received from its own config source; the
// first Propose wins the conditional create and the rest join the same
// generation. The digest recorded in the rollout row is the cross-member
// agreement check: a member whose config source handed it different bytes
// computes a different digest, cannot verify the proposal, and Nacks —
// so a divergent config aborts the rollout instead of splitting the cohort.

const (
	// defaultRolloutTTL bounds how long a proposed rollout may stay unresolved
	// before the coordinator aborts it. The design's sizing (§13 Q2) is
	// 2 × convergence budget floor + member build budget ≈ 5 minutes; NETCONF's
	// confirmed-commit default (600 s) is the same order of magnitude.
	defaultRolloutTTL = 5 * time.Minute
)

// ClusterRolloutConfig wires the coordinated rollout barrier into a Supervisor.
// It is opt-in: without it a clustered live reload keeps the ADR 0012 refusal,
// so an operator who has not configured the barrier sees no behavior change.
type ClusterRolloutConfig struct {
	// Store is the shared, conditional-write rollout store every member
	// coordinates through. Required.
	Store ports.ClusterRolloutStore

	// MemberID identifies this node within the membership epoch. It MUST be
	// stable for the node, distinct across the cohort, and MUST appear verbatim
	// in the applied config's bridge.cluster.members roster — decideRollout
	// compares live membership against the frozen epoch by set equality, so a
	// duplicate or drifting ID aborts every rollout. Required.
	//
	// It is injected rather than read from config because a coordinated cohort
	// shares ONE config document: bridge.instance_id is empty there precisely so
	// each task derives its own runtime identity, and bridge.cluster.endpoints is
	// a capability map, not a roster. Only the composition root knows this node's
	// stable identity (ECS task id, pod name, host), so only it can supply this.
	MemberID string

	// Lease elects the rollout coordinator on a well-known lease id, and its
	// LeaseToken is the fencing token passed to Commit/Abort. Required: a
	// cohort with no coordinator never decides, so every rollout would sit until
	// its TTL and expire. It may be the same LeaseStore the deployment already
	// uses for exclusive routes — the lease id is distinct.
	Lease ports.LeaseStore

	// TTL bounds an unresolved rollout before the coordinator aborts it.
	// Zero selects defaultRolloutTTL.
	TTL time.Duration

	// LeaseTTL bounds how long a crashed coordinator blocks its successor, and
	// doubles as the lock delay a fresh coordinator waits out before its first
	// decision (§6). Zero selects defaultCoordLeaseTTL.
	LeaseTTL time.Duration

	// PollInterval is how often this member re-reads the rollout row.
	// Zero selects defaultRolloutPollInterval.
	PollInterval time.Duration

	// StoreCallTimeout bounds EVERY individual rollout-store and coordinator-lease
	// call the barrier makes. Zero selects defaultRolloutStoreCallTimeout.
	//
	// It exists because the barrier drive is one goroutine: without a bound, a
	// single store call that does not return stops commit, abort, confirm, revert,
	// observation freshness, the local confirm-window deadman and the process
	// shutdown that waits on the drive. Keep it well under LeaseTTL — a tick makes
	// several calls, and their sum must stay inside the TTL or a coordinator can
	// lose its lease while waiting on its own renewal.
	StoreCallTimeout time.Duration

	// Encode and Decode round-trip a *ports.BridgeConfig through the durable
	// last-committed config artifact (design Phase-4 residual): Encode on commit
	// (to persist the committed bytes), Decode on boot / reconcile (to rebuild the
	// committed config a member boots on or converges to). They are INJECTED
	// because bridge must not import config/parser (arch-lint): the composition
	// root supplies config/parser.MarshalBridgeConfigJSON and a registry-bound
	// parser.Parse. Encode MUST be the round-trippable wire form (NOT the canonical
	// digest projection) so Decode can reconstruct the config.
	//
	// Both optional: when either is nil the committed-artifact residual protection
	// is disabled and the joiner keeps the conservative fail-closed refusal for an
	// aborted/undecided boot config. Wire both in a coordinated production
	// deployment.
	Encode func(*ports.BridgeConfig) ([]byte, error)
	Decode func([]byte) (*ports.BridgeConfig, error)
}

// rolloutBarrier is the per-Supervisor proposer state.
type rolloutBarrier struct {
	store ports.ClusterRolloutStore
	// committedStore is the durable last-committed config artifact port. It is the
	// same backing store as `store` when that store implements it (the usual case),
	// discovered by assertion in newRolloutBarrier; nil otherwise, which — like a
	// nil codec — disables the committed-artifact residual protection.
	committedStore ports.ClusterCommittedConfigStore
	lease          ports.LeaseStore
	memberID       string
	ttl            time.Duration
	leaseTTL       time.Duration
	pollInterval   time.Duration

	// encode/decode round-trip a config through the durable committed-config
	// artifact (see ClusterRolloutConfig.Encode/Decode). Both nil disables the
	// committed-artifact residual protection.
	encode func(*ports.BridgeConfig) ([]byte, error)
	decode func([]byte) (*ports.BridgeConfig, error)

	// ops bounds and meters every remote call the barrier makes — the proposer's,
	// the applier's and the coordinator's alike, so a store that has stopped
	// answering is refused for all of them at once (rollout_ops.go).
	ops *rolloutOps

	// candMu guards the staged candidate below.
	candMu sync.Mutex
	// cand holds the candidate this node's OWN config source delivered, staged
	// for the applier by digest (design §5, transport option (b) — see the
	// candidate-transport note at the top of rollout_applier.go). Only one
	// rollout is active at a time, so one slot is enough; a newer candidate
	// overwrites the older one, which by then belongs to a rollout that has
	// already resolved or is about to deadline-abort.
	cand stagedCandidate
}

// stagedCandidate is a candidate config held between its proposal and the
// barrier's decision.
type stagedCandidate struct {
	// digest identifies the candidate in the rollout row.
	digest string
	// frozen is the immutable content the applier verifies, classifies, and
	// builds against.
	frozen *ports.BridgeConfig
	// source is the ORIGINAL pointer the config manager emitted. It exists
	// solely so the commit-time SwapEvent can echo it back: the manager
	// correlates apply results by exact pointer identity (a stale ack must not
	// regress running to an older generation with identical content), so a swap
	// event carrying the frozen clone instead would be discarded as foreign and
	// leave ReconfigurePending — and therefore deep health's Degraded — latched
	// true forever after every successful rollout.
	source *ports.BridgeConfig
}

// stage records the candidate config this node proposed or joined, keyed by its
// digest, so the applier can build and adopt it without re-fetching. frozen is
// already frozen (cloneConfigForBuild) and validated by the node's own
// config-source load path; source is the manager's original pointer.
func (b *rolloutBarrier) stage(digest string, frozen, source *ports.BridgeConfig) {
	b.candMu.Lock()
	defer b.candMu.Unlock()
	b.cand = stagedCandidate{digest: digest, frozen: frozen, source: source}
}

// candidate returns the staged candidate for digest, or ok=false when this node
// has not (yet) seen that candidate through its own config source. Not-staged is
// the normal state for a member whose watcher is lagging: it simply does not
// vote yet, and the rollout deadline bounds the wait.
func (b *rolloutBarrier) candidate(digest string) (stagedCandidate, bool) {
	b.candMu.Lock()
	defer b.candMu.Unlock()
	if digest == "" || digest != b.cand.digest {
		return stagedCandidate{}, false
	}
	return b.cand, true
}

// writeCommittedArtifact durably records the config the cohort committed at
// generation gen as the last-committed artifact (design Phase-4 residual). It is
// idempotent across the cohort — every adopting member writes the same
// (generation, digest) pair, and the store treats a matching re-write as a no-op
// — and a no-op when no codec is wired. The recorded digest is the CANONICAL
// digest (identity, matching the rollout row); the bytes are the round-trippable
// wire form a (re)joining member decodes.
func (b *rolloutBarrier) writeCommittedArtifact(ctx context.Context, gen uint64, version int, cfg *ports.BridgeConfig) error {
	if b.encode == nil || b.committedStore == nil {
		return nil
	}
	raw, err := b.encode(cfg)
	if err != nil {
		return fmt.Errorf("bridge: encoding the committed config for the durable rollout artifact failed: %w", err)
	}
	digest, ok := configCanonicalBytesDigest(cfg)
	if !ok {
		return fmt.Errorf("bridge: cannot compute the committed config digest for the durable rollout artifact")
	}
	return b.ops.run(ctx, rolloutOpArtifact, func(callCtx context.Context) error {
		return b.committedStore.PutCommittedConfig(callCtx, persistence.CommittedRolloutConfig{
			Generation:    gen,
			ConfigVersion: version,
			ConfigBytes:   raw,
			Digest:        digest,
		})
	})
}

// newRolloutBarrier validates the wiring and applies defaults. It returns nil
// when the configuration is incomplete, so a half-wired barrier degrades to the
// ADR 0012 refusal (fail closed) rather than silently proposing nothing.
func newRolloutBarrier(cfg ClusterRolloutConfig) *rolloutBarrier {
	if cfg.Store == nil || cfg.Lease == nil || cfg.MemberID == "" {
		return nil
	}
	b := &rolloutBarrier{
		store:        cfg.Store,
		lease:        cfg.Lease,
		memberID:     cfg.MemberID,
		ttl:          orDefault(cfg.TTL, defaultRolloutTTL),
		leaseTTL:     orDefault(cfg.LeaseTTL, defaultCoordLeaseTTL),
		pollInterval: orDefault(cfg.PollInterval, defaultRolloutPollInterval),
		encode:       cfg.Encode,
		decode:       cfg.Decode,
		ops:          newRolloutOps(cfg.StoreCallTimeout),
	}
	// The committed-config artifact is a separate small port; the same backing
	// store usually implements it. Discover that capability rather than adding a
	// second public wiring field for what is almost always one object.
	if cs, ok := cfg.Store.(ports.ClusterCommittedConfigStore); ok {
		b.committedStore = cs
	}
	return b
}

// orDefault selects fallback for a non-positive duration.
func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// rolloutMembers resolves the membership epoch from the applied config's static
// bridge.cluster.members roster. It is the ONE membership source: the proposer
// freezes the epoch from it and the coordinator compares live membership against
// it, so the two cannot disagree and report a spurious membership change on
// every rollout.
//
// It returns nil when the roster is absent — the caller MUST fail closed rather
// than propose against a guessed epoch. Duplicates are NOT removed here:
// persistence.NewRollout sorts and dedups the epoch it freezes, and decideRollout
// dedups the live roster before comparing, so a duplicate is inert either way.
func rolloutMembers(cfg *ports.BridgeConfig) []string {
	if cfg == nil || cfg.Bridge.Cluster == nil {
		return nil
	}
	return slices.Clone(cfg.Bridge.Cluster.Members)
}

// sortedSet renders member ids as the canonical set the barrier compares on:
// sorted and deduplicated. persistence.NewRollout stores the frozen epoch in
// exactly this form, so every comparison against it — the coordinator's live
// membership check and the replacement-required roster check — must
// normalise the same way or a reorder reads as a membership change.
func sortedSet(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// proposeCoordinatedRollout hands a live-safe clustered delta to the rollout
// barrier. It returns an error whenever the delta could NOT be proposed, and the
// caller MUST then fail closed — an unproposed delta that is reported as
// deferred would be acknowledged as committed while no member ever applies it.
//
// Fail-closed cases: no barrier wired (the operator opted into coordinated
// rollout in config but the process has no rollout store); an undeterminable
// membership epoch — proposing against a guessed epoch would let the coordinator
// commit without every real member's ack; this node absent from its own epoch —
// a rollout it can never Ack; and a collision with a DIFFERENT in-flight rollout
// (see joinActive).
// sourceCfg is the ORIGINAL pointer the config manager emitted; it is staged
// alongside the frozen candidate so the commit-time SwapEvent can echo it back
// (see stagedCandidate.source).
func (d *ClusterRolloutDriver) Propose(ctx context.Context, oldCfg, newCfg, sourceCfg *ports.BridgeConfig) error {
	b := d.barrier
	members := rolloutMembers(oldCfg)
	if len(members) == 0 {
		return fmt.Errorf("bridge: cluster.rollout: coordinated is configured but the cohort roster "+
			"bridge.cluster.members is empty, so a rollout cannot freeze a membership epoch and the "+
			"live reload is refused (the running config keeps serving) "+
			"(attempted_config_version=%d)", newCfg.Version)
	}
	if !slices.Contains(members, b.memberID) {
		return fmt.Errorf("bridge: this node's rollout member id %q is not in the cohort membership "+
			"epoch %v, so it could never Ack its own proposal (persistence.Rollout rejects a voter "+
			"outside the frozen epoch) and the rollout could only deadline-abort while blocking every "+
			"other proposal for its whole TTL. The live reload is refused (the running config keeps "+
			"serving); align bridge.WithClusterRollout's MemberID with bridge.cluster.members "+
			"(attempted_config_version=%d)", b.memberID, members, newCfg.Version)
	}
	// The confirm window governing this rollout comes from the RUNNING config
	// (oldCfg): every member currently runs it and reads the same value, so the
	// window frozen in the proposal is deterministic across the cohort. A change TO
	// the window is a cluster.* delta, which is replacement-required, so old and new
	// agree for any live-safe delta anyway.
	confirmWindow := clusterConfirmWindow(oldCfg)
	if err := b.propose(ctx, newCfg, sourceCfg, members, confirmWindow); err != nil {
		return fmt.Errorf("bridge: proposing the coordinated cluster rollout failed, so the live reload "+
			"is refused and the running config keeps serving (attempted_config_version=%d): %w",
			newCfg.Version, err)
	}
	return nil
}

// clusterConfirmWindow reads the coordinated-rollout confirm window (design §8.1)
// from a config, or 0 when absent — the base protocol.
func clusterConfirmWindow(cfg *ports.BridgeConfig) time.Duration {
	if cfg == nil || cfg.Bridge.Cluster == nil {
		return 0
	}
	return cfg.Bridge.Cluster.ConfirmWindowDuration()
}

// propose stages newCfg as a candidate generation. It is idempotent across the
// cohort: when another member already proposed THIS SAME candidate the
// conditional create fails with shared.ErrAlreadyExists and this node simply
// joins the generation the peer opened. Any other error — including a collision
// with a DIFFERENT active rollout — fails closed.
func (b *rolloutBarrier) propose(ctx context.Context, newCfg, sourceCfg *ports.BridgeConfig, members []string, confirmWindow time.Duration) error {
	digest, ok := configCanonicalBytesDigest(newCfg)
	if !ok {
		return fmt.Errorf("bridge: cannot compute the candidate config digest for a coordinated rollout")
	}
	err := b.ops.run(ctx, rolloutOpPropose, func(callCtx context.Context) error {
		_, perr := b.store.Propose(callCtx, persistence.RolloutProposal{
			ProposerID:    b.memberID,
			ConfigDigest:  digest,
			ConfigVersion: newCfg.Version,
			Members:       members,
			TTL:           b.ttl,
			ConfirmWindow: confirmWindow,
		})
		return perr
	})
	if err != nil {
		if !errors.Is(err, shared.ErrAlreadyExists) {
			return err
		}
		if joinErr := b.joinActive(ctx, digest); joinErr != nil {
			return joinErr
		}
	}
	// Staged only on success: a delta that could not be proposed or joined must
	// not be adoptable by the applier — the guard fails it closed, and a staged
	// candidate for a rollout this node is not part of would be adopted the
	// moment some OTHER rollout happened to carry the same digest.
	b.stage(digest, newCfg, sourceCfg)
	return nil
}

// joinActive resolves a Propose that lost to an already-active rollout. The
// store's ErrAlreadyExists answers "a rollout is active" (invariant is
// per-store), NOT "your rollout is active" — so it MUST be disambiguated by
// reading the active row back:
//
//   - same candidate digest → a peer won the conditional create for this exact
//     delta and this node joins that generation: success.
//   - any other digest → a DIFFERENT change is in flight. Reporting success here
//     would defer this delta against a barrier that commits somebody else's
//     config: the admin API resolves a deferral as committed-not-applied (no
//     rollback) and the delta then never applies on any member — a silent drop.
//     Fail closed; the operator retries once the in-flight rollout resolves.
func (b *rolloutBarrier) joinActive(ctx context.Context, digest string) error {
	r, err := rolloutOpValue(ctx, b.ops, rolloutOpRead, b.store.Current)
	if err != nil {
		return fmt.Errorf("bridge: a cluster rollout is already active but it could not be read back to "+
			"check whether it carries this delta, so the change cannot be safely deferred to it: %w", err)
	}
	if r.ConfigDigest() != digest {
		return fmt.Errorf("bridge: a DIFFERENT cluster config rollout is already in flight "+
			"(generation=%d state=%q config_version=%d); one rollout at a time is the barrier's "+
			"invariant, so this delta was NOT proposed. Wait for the in-flight rollout to commit or "+
			"abort, then retry", r.Generation(), r.State(), r.ConfigVersion())
	}
	if r.State() != persistence.RolloutProposed && r.State() != persistence.RolloutStaging {
		// Already committed (provisionally or finally), confirmed, reverted, or
		// aborted — the vote phase for this generation is over, so this node cannot
		// join it. Retry once it resolves.
		return fmt.Errorf("bridge: the rollout carrying this delta (generation=%d) already reached %q "+
			"before this node joined it; retry the change", r.Generation(), r.State())
	}
	// Same digest, but the peer froze the epoch from ITS roster. If this node is
	// not in that epoch it can never Ack (the aggregate rejects a voter outside
	// the epoch), so joining would defer forever behind a rollout guaranteed to
	// deadline-abort — the same dead-end the proposer path refuses up front.
	if !slices.Contains(r.MembershipEpoch(), b.memberID) {
		return fmt.Errorf("bridge: a peer proposed this delta (generation=%d) with a membership epoch %v "+
			"that excludes this node %q, so this node could never Ack it; the cohort's members disagree "+
			"about the roster — reconcile bridge.cluster.endpoints across the cohort and retry",
			r.Generation(), r.MembershipEpoch(), b.memberID)
	}
	return nil
}

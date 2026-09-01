package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The joiner rule (design §6): "a starting member adopts only the last Committed
// configuration; it never acks a rollout proposed before it joined".
//
// It exists because the candidate travels through each member's OWN config
// source (see the transport note in rollout_applier.go). The operator's change
// is durably written to that source BEFORE the barrier decides, so between the
// write and the decision the source's current document is a config the cohort
// has NOT agreed to run. Without this gate a member restarting in that window —
// or at any time after an ABORTED rollout, since the rejected document stays in
// the source until the operator rolls it back — would boot straight onto a
// config no other member is running. That is the mixed-version cohort goal
// forbids and, since ADR 0012 refuses clustered live reload outright today, it
// would be a safety REGRESSION rather than a limitation.
//
// The rule is deliberately narrow: it refuses only what the barrier has
// explicitly not-committed. In particular it does NOT require the boot config to
// equal the last committed digest, because the whole-cohort replacement
// procedure — still the mandated path for replacement-required deltas (§8) —
// legitimately boots every member onto a config no rollout ever carried.

// resolveCoordinatedBoot decides which config a coordinated member actually
// boots on and returns it (build-ready). It is the durable-artifact evolution of
// the joiner rule: rather than only APPROVING or REFUSING the boot config, it can
// substitute the durable last-committed config when the boot config is a
// candidate the barrier has not committed — closing the restart-into-window and
// restart-after-abort residuals without the availability cost of a refusal.
//
// It falls back to the conservative checkCoordinatedRolloutPreflight (refuse an
// aborted/undecided boot config) whenever the deployment is not coordinated, no
// barrier is wired, or the committed-artifact codec is not wired — so a
// deployment that has not opted into the durable artifact is unaffected.
func (d *ClusterRolloutDriver) ResolveBoot(ctx context.Context, cfg *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	if d.barrier == nil || !coordinatedRollout(cfg) ||
		d.barrier.decode == nil || d.barrier.encode == nil || d.barrier.committedStore == nil {
		if err := d.checkCoordinatedRolloutPreflight(ctx, cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if err := d.checkRolloutMembership(cfg); err != nil {
		return nil, err
	}
	return d.resolveBootFromCommittedArtifact(ctx, cfg)
}

// resolveBootFromCommittedArtifact is the committed-artifact boot resolution (see
// resolveCoordinatedBoot). It seeds the baseline on a fresh cohort, boots the
// config unchanged when it IS the committed one or a whole-cohort replacement,
// and otherwise substitutes the durable committed config so the member never runs
// a config the barrier has not committed.
func (d *ClusterRolloutDriver) resolveBootFromCommittedArtifact(ctx context.Context, cfg *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	bootDigest, ok := configCanonicalBytesDigest(cfg)
	if !ok {
		return nil, fmt.Errorf("bridge: cannot compute the boot config digest to check it against the " +
			"cluster rollout committed artifact; refusing to start")
	}
	committed, err := rolloutOpValue(ctx, d.barrier.ops, rolloutOpRead, d.barrier.committedStore.CommittedConfig)
	if errors.Is(err, shared.ErrNotFound) {
		// No committed artifact yet: no barrier rollout has ever committed, so there
		// is no cohort-committed config to recover to. Do NOT seed off `current` —
		// in the write→propose window `current` holds an un-proposed CANDIDATE, and
		// a member cannot locally tell that from a deploy baseline, so seeding it
		// would durably poison the baseline and split the cohort. Fall back to the
		// conservative joiner rule (refuse an aborted/undecided boot config, boot on
		// current otherwise) until the first commit establishes the artifact. (An
		// explicit deploy-time seed is a Phase-6 composition concern.)
		if jerr := d.checkRolloutJoinerRule(ctx, cfg); jerr != nil {
			return nil, jerr
		}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bridge: cluster.rollout: the durable last-committed config artifact could "+
			"not be read at startup, so this node cannot tell whether its boot config is the one the cohort "+
			"committed; refusing to start (config_version=%d): %w", cfg.Version, err)
	}
	if committed.Digest == bootDigest {
		return cfg, nil // the boot config IS the last committed config
	}
	// The boot config differs from the committed artifact. Reconstruct the
	// committed config; fail closed if it cannot be rebuilt — booting on the wrong
	// config is the split-brain the whole protocol prevents.
	committedCfg, err := d.barrier.decode(committed.ConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("bridge: cluster.rollout: the durable last-committed config artifact "+
			"(generation=%d config_version=%d) could not be decoded, so this node cannot recover the "+
			"config the cohort is running; refusing to start: %w", committed.Generation, committed.ConfigVersion, err)
	}
	// Integrity: the reconstructed committed config must match the digest the
	// artifact records, mirroring reconcileMissedCommit. A
	// decodable-but-wrong artifact (bit-rot that still parses, or a non-digest-
	// preserving codec) must not be booted as if it were the committed config.
	if raw, ok := configCanonicalBytes(committedCfg); !ok || candidateConfigDigest(raw) != committed.Digest {
		return nil, fmt.Errorf("bridge: cluster.rollout: the durable last-committed config artifact "+
			"(generation=%d) failed its digest check after decoding, so its bytes are not the config the "+
			"cohort committed; refusing to start", committed.Generation)
	}
	d.stageBootConfig(cfg, bootDigest)
	// A replacement-required delta cannot roll through the barrier. Boot on the
	// new config ONLY when it is strictly NEWER than the committed artifact — a
	// forward whole-cohort replacement. A same-or-older replacement-delta is a
	// stale or rolled-back boot config (the member's config source is lagging), so
	// boot on the committed config instead of running a config no peer runs.
	// A live-safe delta always belongs to the barrier: boot on the committed
	// config and let the barrier roll the candidate `cfg` (staged above).
	if class, _ := classifyRolloutDelta(committedCfg, cfg); class == rolloutReplacementRequired &&
		cfg.Version > committed.ConfigVersion {
		return cfg, nil
	}
	frozen, err := cloneConfigForBuild(committedCfg)
	if err != nil {
		return nil, fmt.Errorf("bridge: cluster.rollout: freezing the decoded committed config for boot "+
			"failed; refusing to start (committed generation=%d): %w", committed.Generation, err)
	}
	return frozen, nil
}

// checkCoordinatedRolloutPreflight is the startup gate for a coordinated
// clustered deployment with a rollout store wired; every other shape is
// unaffected and returns nil. It runs before anything is built, so a
// misconfigured or unsafe boot fails loudly at startup rather than at the first
// live reload — which is to say, during a change, at night.
func (d *ClusterRolloutDriver) checkCoordinatedRolloutPreflight(ctx context.Context, cfg *ports.BridgeConfig) error {
	if d.barrier == nil || !coordinatedRollout(cfg) {
		return nil
	}
	if err := d.checkRolloutMembership(cfg); err != nil {
		return err
	}
	return d.checkRolloutJoinerRule(ctx, cfg)
}

// checkRolloutMembership requires this node's barrier member id to appear in the
// cohort roster. The blueprint validator cannot check this — the member id is
// composition-root wiring, not config — so admission happens here.
//
// A node outside its own roster can never Ack (the aggregate rejects a voter
// outside the frozen epoch), so every rollout it proposes is guaranteed to
// deadline-abort while blocking every other proposal for the whole TTL
// (invariant permits one active rollout). Catching it at boot turns a 3am
// discovery into a startup failure.
func (d *ClusterRolloutDriver) checkRolloutMembership(cfg *ports.BridgeConfig) error {
	members := rolloutMembers(cfg)
	if len(members) == 0 {
		return fmt.Errorf("bridge: cluster.rollout: coordinated requires a non-empty "+
			"bridge.cluster.members roster — it is the membership epoch the rollout barrier freezes "+
			"and counts acknowledgements against, and an empty roster would let the barrier commit "+
			"with ZERO acknowledgements. Refusing to start (config_version=%d)", cfg.Version)
	}
	if !slices.Contains(members, d.barrier.memberID) {
		return fmt.Errorf("bridge: this node's rollout member id %q does not appear in the cohort "+
			"roster bridge.cluster.members %v, so it could never acknowledge a rollout and every "+
			"change it proposes would deadline-abort while blocking the cohort's next change for the "+
			"whole rollout TTL. Align bridge.WithClusterRollout's MemberID with the roster. Refusing "+
			"to start (config_version=%d)", d.barrier.memberID, members, cfg.Version)
	}
	return nil
}

// checkRolloutJoinerRule reports whether this node may boot on cfg.
//
// It fails CLOSED on a store error. The rollout store is a hard requirement of
// cluster.rollout: coordinated (a member cannot participate in the barrier
// without it), so an unreadable store means this node cannot tell a committed
// config from a rejected one — and booting on the wrong one splits the cohort.
func (d *ClusterRolloutDriver) checkRolloutJoinerRule(ctx context.Context, cfg *ports.BridgeConfig) error {
	r, err := rolloutOpValue(ctx, d.barrier.ops, rolloutOpRead, d.barrier.store.Current)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return nil // no rollout has ever been proposed: nothing has been rejected
		}
		return fmt.Errorf("bridge: cluster.rollout: coordinated requires the rollout store to be "+
			"readable at startup so this node can tell a cohort-committed config from one the cohort "+
			"rejected; booting without that check could start this node on a config no other member "+
			"is running (a mixed-version cohort). Refusing to start (config_version=%d): %w",
			cfg.Version, err)
	}
	digest, ok := configCanonicalBytesDigest(cfg)
	if !ok {
		return fmt.Errorf("bridge: cannot compute the boot config digest to check it against the " +
			"cluster rollout barrier; refusing to start")
	}
	if r.ConfigDigest() != digest {
		// The boot config is not the one this rollout carries, so this rollout
		// says nothing about it. An in-flight rollout for a DIFFERENT candidate
		// is the normal case for a member restarting mid-rollout: it boots the
		// old committed config and votes on the candidate through the applier.
		d.stageBootConfig(cfg, digest)
		return nil
	}
	switch r.State() {
	case persistence.RolloutCommitted:
		return nil // the cohort agreed to run exactly this config
	case persistence.RolloutAborted:
		return fmt.Errorf("bridge: refusing to start on a config the cluster rollout barrier ABORTED "+
			"(generation=%d config_version=%d reason=%q). The cohort is running the previous config, so "+
			"starting on this one would split the cluster across config versions. Roll the config source "+
			"back to the last committed document (or fix and re-propose the change), then start this "+
			"node; see docs/runbooks/cluster-config-rollout.md",
			r.Generation(), r.ConfigVersion(), r.Reason())
	default:
		return fmt.Errorf("bridge: refusing to start on a config whose cluster rollout is still "+
			"UNDECIDED (generation=%d state=%q config_version=%d). Starting now would apply the "+
			"candidate ahead of the all-member commit barrier — exactly the mixed-version cohort the "+
			"barrier prevents. Wait for the rollout to commit or abort, then start this node; "+
			"see docs/runbooks/cluster-config-rollout.md",
			r.Generation(), r.State(), r.ConfigVersion())
	}
}

// stageBootConfig stages the config this node booted on as its candidate.
//
// It exists because staging otherwise happens ONLY in the proposer path
// (rolloutBarrier.propose), which a booting member never runs: it receives its
// config as Run's `initial`, not as a change through Supervisor.apply. Without
// this, a member that restarted onto a document its peers have not yet proposed
// could never vote on the rollout that later carries that exact document — its
// own watcher will not re-deliver unchanged content, so the candidate would
// never be staged and every such rollout would deadline-abort with that member
// permanently silent.
//
// Staging is safe here for the same reason it is safe in the proposer: the
// content came through this node's own validated config-source load path, and
// the applier still verifies the digest and classifies the delta before voting.
func (d *ClusterRolloutDriver) stageBootConfig(cfg *ports.BridgeConfig, digest string) {
	frozen, err := cloneConfigForBuild(cfg)
	if err != nil {
		return // unfreezable config; the boot itself will fail on it shortly
	}
	d.barrier.stage(digest, frozen, cfg)
}

// configCanonicalBytesDigest is the candidate digest of cfg, or ok=false when
// cfg cannot be canonicalised (the caller must then fail closed).
func configCanonicalBytesDigest(cfg *ports.BridgeConfig) (string, bool) {
	raw, ok := configCanonicalBytes(cfg)
	if !ok {
		return "", false
	}
	return candidateConfigDigest(raw), true
}

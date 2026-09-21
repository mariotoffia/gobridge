package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The generation-zero baseline (docs/cluster/spec/cluster-config-rollout-protocol.md).
// Seeding one explicitly at deploy time is a composition-root concern, not the
// barrier's: this package only has to behave when no baseline exists yet.
//
// Before the first rollout ever commits there is no durable committed artifact,
// so resolveBootFromCommittedArtifact has nothing to recover to and falls back to
// the conservative joiner rule — which boots the member on whatever its own
// config source currently holds. In the window between an operator writing a
// change to that source and the barrier proposing it, "whatever the source holds"
// is a candidate the cohort has NOT agreed to run, and a member restarting there
// would run a generation no member agreed on — the one thing the barrier makes
// impossible rather than merely bounded (ADR 0013).
//
// The barrier cannot close that itself: a member cannot locally tell a deploy
// baseline from an un-proposed candidate, which is why the joiner deliberately
// refuses to seed off `current`. Only the composition root knows which document
// its DEPLOYMENT admitted. So the driver supplies the mechanism and the
// composition root supplies the policy: it seeds the baseline only for the
// config document its deployment stamped, before the process becomes ready.

// ConfigArtifactDigest returns the content identity of cfg — the exact value
// the rollout row records for a candidate and the committed-config artifact
// records for a commit. File-source deployments also stamp it to recognize the
// content they admitted at startup.
//
// It is taken over the content normal form (ADR 0016), so it leaves the version
// number out and is unmoved by how the document happens to be written: the order
// of the id-keyed lists, the spelling of a duration, and a default written out
// rather than left out all give the same value. A real change to what the bridge
// runs gives a different one.
//
// It is a pure function of the config document (see candidateConfigDigest), so
// two members that loaded the same document compute the same value.
func ConfigArtifactDigest(cfg *ports.BridgeConfig) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("bridge: cannot compute the artifact digest of a nil config")
	}
	digest, ok := configCanonicalBytesDigest(cfg)
	if !ok {
		return "", fmt.Errorf("bridge: the config could not be canonicalised, so it has no artifact digest")
	}
	return digest, nil
}

// DeploymentBaselineContentDigest identifies deployment-admitted content when
// the config source assigns its own version number, as the DynamoDB seeder does.
//
// It is the SAME value as ConfigArtifactDigest: since the content identity is
// taken over the normal form, which leaves the version out, there is no longer
// anything for this function to do on top of it. The name is kept because the
// AWS deployment stamps it into the running infrastructure — the CDK construct
// writes it as dynamodb_ha_baseline_config_digest — and a member reads that
// stamp back to recognize the document its deployment admitted.
func DeploymentBaselineContentDigest(cfg *ports.BridgeConfig) (string, error) {
	return ConfigArtifactDigest(cfg)
}

// CommittedBaseline reports the cohort's durable committed-config artifact as it
// stands right now: the generation and digest a restart of this member would
// recover to. It is the read-only half of SeedBaseline, for a member whose own
// document is NOT the deployment baseline (the usual case once the cohort has
// rolled a change) and which therefore has nothing to seed but still has a
// recovery point worth publishing in health.
//
// shared.ErrNotFound means no baseline has been established yet.
func (d *ClusterRolloutDriver) CommittedBaseline(ctx context.Context) (uint64, string, error) {
	if d == nil || d.barrier == nil || d.barrier.committedStore == nil {
		return 0, "", fmt.Errorf("bridge: cluster.rollout: no durable committed-config artifact is wired, " +
			"so this member has no recorded baseline to recover to")
	}
	committed, err := rolloutOpValue(ctx, d.barrier.ops, rolloutOpRead, d.barrier.committedStore.CommittedConfig)
	if err != nil {
		return 0, "", err
	}
	return committed.Generation, committed.Digest, nil
}

// SeedBaseline durably records cfg as the cohort's generation-zero committed
// config artifact and VERIFIES the result by reading it back. It returns the
// generation and digest the artifact actually holds afterwards.
//
// It is safe to call on every boot, for the life of the cohort, because the
// store's monotonicity rules do the deciding:
//
//   - fresh cohort: generation zero is written, and every member of the cohort
//     writing the same baseline is an idempotent no-op rather than a conflict;
//   - after a rollout committed: the generation-zero write is a lower-generation
//     no-op, and the read-back tells the caller which generation the cohort is
//     actually on — the seed can never rewind the cohort to its deploy state;
//   - divergent baseline: a DIFFERENT config already at generation zero means the
//     cohort established its baseline from another document (a peer's, or an
//     earlier deploy's). The ESTABLISHED baseline wins — this call does not
//     overwrite it and does not fail — and the read-back reports it, so the
//     caller learns what this member would actually recover to. Overwriting would
//     retarget what every peer recovers to; refusing would mean a redeploy that
//     changes the config could never start again, since no member could tell its
//     own new baseline apart from a divergent one.
//
// The read-back is load-bearing for the same reason it is on the commit path: a
// no-op success is indistinguishable from a write that landed, so only a read
// proves the cohort has a durable baseline to recover to at all.
func (d *ClusterRolloutDriver) SeedBaseline(ctx context.Context, cfg *ports.BridgeConfig) (uint64, string, error) {
	if d == nil || d.barrier == nil {
		return 0, "", fmt.Errorf("bridge: cluster.rollout: no barrier is wired, so the generation-zero " +
			"baseline cannot be seeded")
	}
	if d.barrier.committedStore == nil || d.barrier.encode == nil {
		return 0, "", fmt.Errorf("bridge: cluster.rollout: seeding the generation-zero baseline needs the " +
			"durable committed-config artifact — wire the codec (ClusterRolloutConfig.Encode/Decode) on a " +
			"rollout store that implements ports.ClusterCommittedConfigStore")
	}
	if cfg == nil {
		return 0, "", fmt.Errorf("bridge: cluster.rollout: cannot seed a nil config as the " +
			"generation-zero baseline")
	}
	if err := d.barrier.writeCommittedArtifact(ctx, 0, cfg.Version, cfg); err != nil &&
		!errors.Is(err, shared.ErrRolloutDigestMismatch) {
		return 0, "", fmt.Errorf("bridge: cluster.rollout: seeding the generation-zero baseline failed, so "+
			"this cohort has no durable config a restarting member could recover to: %w", err)
	}
	committed, err := rolloutOpValue(ctx, d.barrier.ops, rolloutOpRead, d.barrier.committedStore.CommittedConfig)
	if err != nil {
		return 0, "", fmt.Errorf("bridge: cluster.rollout: the generation-zero baseline could not be read "+
			"back after seeding, so this member cannot prove the cohort has a durable baseline: %w", err)
	}
	return committed.Generation, committed.Digest, nil
}

package bridge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The generation-zero baseline closes the window in which a restarting member
// has no cohort-committed config to recover to: before the first rollout ever
// commits, the durable artifact does not exist, so the joiner has to fall back
// to whatever the member's own config source currently holds — which in the
// write-before-propose window is a candidate the cohort has not agreed to run.
// Seeding the deployment's own admitted config as generation zero gives the
// joiner something durable to recover to from the first boot onward.

// baselineDriver builds a non-running driver with the committed-artifact store
// and codec wired, which is what SeedBaseline requires.
func baselineDriver(t *testing.T, store ports.ClusterRolloutStore, codec *configCodecFake) *ClusterRolloutDriver {
	t.Helper()
	rc := testRolloutConfig(store, "node-a")
	rc.Encode, rc.Decode = codec.encode, codec.decode
	d := NewClusterRolloutDriver(newFakeRolloutHost(nil), rc)
	require.NotNil(t, d)
	return d
}

// TestSeedBaseline_EstablishesGenerationZero proves the seed writes and VERIFIES
// the durable artifact: after it returns, a member that restarts has a
// cohort-committed config to recover to even though no rollout has ever run.
func TestSeedBaseline_EstablishesGenerationZero(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	d := baselineDriver(t, store, codec)
	cfg := coordinatedClusteredCfg("r1")

	gen, digest, err := d.SeedBaseline(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), gen, "a fresh cohort's baseline is generation zero")
	want, ok := configCanonicalBytesDigest(cfg)
	require.True(t, ok)
	assert.Equal(t, want, digest, "the verified digest is the config's canonical identity")

	committed, err := store.CommittedConfig(context.Background())
	require.NoError(t, err, "the baseline must be durable, not just returned")
	assert.Equal(t, want, committed.Digest)
}

// TestSeedBaseline_IsIdempotentAcrossMembers proves every member of the cohort
// may seed the same baseline on boot: the store treats a same-generation,
// same-digest write as a no-op success, so N members racing at deploy time do
// not conflict.
func TestSeedBaseline_IsIdempotentAcrossMembers(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	cfg := coordinatedClusteredCfg("r1")

	for _, member := range []string{"node-a", "node-b", "node-a"} {
		rc := testRolloutConfig(store, member)
		rc.Encode, rc.Decode = codec.encode, codec.decode
		d := NewClusterRolloutDriver(newFakeRolloutHost(nil), rc)
		_, _, err := d.SeedBaseline(context.Background(), cfg)
		require.NoErrorf(t, err, "member %s must be able to seed the same baseline", member)
	}
}

// TestSeedBaseline_EstablishedBaselineWins is the baseline-conflict rule. Two
// DIFFERENT configs cannot share generation zero, and the one already there is
// the cohort's answer: seeding another must neither overwrite it (that would
// retarget what every peer recovers to) nor fail the member (a redeploy that
// changes the config cannot tell its own new baseline from a divergent one, so
// failing would mean the cohort could never start again). The caller learns which
// baseline actually stands from the returned digest.
func TestSeedBaseline_EstablishedBaselineWins(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	d := baselineDriver(t, store, codec)
	established := coordinatedClusteredCfg("r1")
	require.NoError(t, seedErr(d, established))
	establishedDigest, ok := configCanonicalBytesDigest(established)
	require.True(t, ok)

	other := coordinatedClusteredCfg("r1")
	other.Bindings[0].Address = "addr/divergent"
	gen, digest, err := d.SeedBaseline(context.Background(), other)
	require.NoError(t, err, "an established baseline is an answer, not a failure")
	assert.Equal(t, uint64(0), gen)
	assert.Equal(t, establishedDigest, digest, "the caller must learn which baseline actually stands")

	committed, err := store.CommittedConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, establishedDigest, committed.Digest, "the established baseline must not be overwritten")
}

// TestSeedBaseline_DoesNotRegressACommittedGeneration proves the seed is safe to
// run on every boot for the life of the cohort: once a rollout has committed,
// the generation-zero write is a no-op and the member learns the artifact it
// would actually boot on, rather than rewinding the cohort to its deploy state.
func TestSeedBaseline_DoesNotRegressACommittedGeneration(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	d := baselineDriver(t, store, codec)

	rolled := coordinatedClusteredCfg("r1")
	rolled.Bindings[0].Address = "addr/rolled"
	rolledDigest, ok := configCanonicalBytesDigest(rolled)
	require.True(t, ok)
	require.NoError(t, store.PutCommittedConfig(context.Background(), persistence.CommittedRolloutConfig{
		Generation: 7, ConfigVersion: 7, ConfigBytes: codec.register(rolled), Digest: rolledDigest,
	}))

	gen, digest, err := d.SeedBaseline(context.Background(), coordinatedClusteredCfg("r1"))
	require.NoError(t, err)
	assert.Equal(t, uint64(7), gen, "the seed must never rewind a committed generation")
	assert.Equal(t, rolledDigest, digest, "the member learns the artifact it would actually boot on")
}

// TestSeedBaseline_RequiresTheCommittedArtifactCodec proves the seed refuses
// rather than silently doing nothing when the deployment has not wired the
// durable committed-config artifact — a caller that believed it had established
// a baseline would leave the restart window open.
func TestSeedBaseline_RequiresTheCommittedArtifactCodec(t *testing.T) {
	d := NewClusterRolloutDriver(newFakeRolloutHost(nil), testRolloutConfig(memoryrollout.NewStore(), "node-a"))
	require.NotNil(t, d)

	_, _, err := d.SeedBaseline(context.Background(), coordinatedClusteredCfg("r1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "codec")
}

// TestResolveBoot_SubstitutesTheSeededBaseline is the write-before-propose
// restart proof: with a baseline seeded, a member restarting onto config bytes
// its own source holds but the cohort has not committed boots the BASELINE, not
// the uncommitted candidate. Without the seed the artifact would be absent and
// the member would boot the candidate.
func TestResolveBoot_SubstitutesTheSeededBaseline(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	d := baselineDriver(t, store, codec)

	baseline := coordinatedClusteredCfg("r1")
	require.NoError(t, seedErr(d, baseline))

	// The operator's edit landed in the config source but no rollout carries it.
	uncommitted := coordinatedClusteredCfg("r1")
	uncommitted.Version = 2
	uncommitted.Bindings[0].Address = "addr/edited"

	got, err := d.ResolveBoot(context.Background(), uncommitted)
	require.NoError(t, err)
	assert.True(t, configContentEqual(got, baseline),
		"a member must boot the cohort's committed baseline, never uncommitted source bytes")
}

// seedErr runs SeedBaseline and keeps only its error, for preconditions.
func seedErr(d *ClusterRolloutDriver, cfg *ports.BridgeConfig) error {
	_, _, err := d.SeedBaseline(context.Background(), cfg)
	return err
}

// TestCommittedBaseline_ReportsTheRecoveryPoint proves the read-only half: a
// member whose own document is not the deployment baseline still learns which
// artifact it would recover to, so health reports a recovery point instead of
// none.
func TestCommittedBaseline_ReportsTheRecoveryPoint(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	d := baselineDriver(t, store, codec)

	_, _, err := d.CommittedBaseline(context.Background())
	require.ErrorIs(t, err, shared.ErrNotFound, "a cohort with no baseline reports not-found, not a failure")

	baseline := coordinatedClusteredCfg("r1")
	require.NoError(t, seedErr(d, baseline))
	want, ok := configCanonicalBytesDigest(baseline)
	require.True(t, ok)

	gen, digest, err := d.CommittedBaseline(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(0), gen)
	assert.Equal(t, want, digest)
}

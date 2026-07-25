package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// configCodecFake round-trips a *ports.BridgeConfig through the durable
// committed-config artifact without real serialization. It keys configs by their
// canonical digest — exactly the identity the artifact records — so encode/decode
// are a faithful stand-in for the injected parser.MarshalBridgeConfigJSON <->
// parser.Parse pair (whose real round-trip is proven in the UC-CR proofs, which
// run a real composition root). encode registers the config it is handed so a
// later decode of those bytes returns it.
type configCodecFake struct {
	byKey map[string]*ports.BridgeConfig
}

func newConfigCodecFake() *configCodecFake {
	return &configCodecFake{byKey: map[string]*ports.BridgeConfig{}}
}

func (c *configCodecFake) encode(cfg *ports.BridgeConfig) ([]byte, error) {
	d, ok := configCanonicalBytesDigest(cfg)
	if !ok {
		return nil, errors.New("configCodecFake: uncanonicalisable config")
	}
	c.byKey[d] = cfg
	return []byte(d), nil
}

func (c *configCodecFake) decode(b []byte) (*ports.BridgeConfig, error) {
	cfg, ok := c.byKey[string(b)]
	if !ok {
		return nil, errors.New("configCodecFake: unknown committed config bytes")
	}
	return cfg, nil
}

// register pre-loads a config so decode can return it without an encode first
// (used to seed a committed artifact a peer wrote).
func (c *configCodecFake) register(cfg *ports.BridgeConfig) []byte {
	b, err := c.encode(cfg)
	if err != nil {
		panic(err)
	}
	return b
}

// newApplierFixtureCodec is newApplierFixture with the committed-config codec
// wired, so adopt writes the durable artifact and reconcile can read it.
func newApplierFixtureCodec(t *testing.T, memberID string, codec *configCodecFake) *applierFixture {
	t.Helper()
	store := memoryrollout.NewStore()
	onSwap, swaps := swapChan(4)
	rc := testRolloutConfig(store, memberID)
	rc.PollInterval = time.Hour
	rc.LeaseTTL = time.Hour
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	s := newTestSupervisor(WithOnSwap(onSwap), WithClusterRollout(rc))

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), changes)
	t.Cleanup(func() { cancel(); <-errCh })

	return &applierFixture{
		sup:     s,
		store:   store,
		applier: &rolloutApplier{sup: s, store: store, memberID: memberID},
		changes: changes,
		swaps:   swaps,
	}
}

// TestRolloutApplier_CommitWritesCommittedArtifact proves the durable
// last-committed artifact is written when a member adopts a committed generation:
// the bytes a (re)joining member later boots on and a member that missed the
// commit reconciles to. Without this write the artifact never exists and every
// residual sequence stays open.
func TestRolloutApplier_CommitWritesCommittedArtifact(t *testing.T) {
	codec := newConfigCodecFake()
	f := newApplierFixtureCodec(t, "node-a", codec)
	cand := liveSafeCandidate(99)
	r := f.propose(t, cand)
	commitAs(t, f.store, r.Generation(), "node-a", "node-b")

	require.NoError(t, f.applier.step(context.Background()))
	require.Equal(t, 99, f.sup.Config().Version, "precondition: the generation was adopted")

	committed, err := f.store.CommittedConfig(context.Background())
	require.NoError(t, err, "adopting a committed generation must write the durable artifact")
	assert.Equal(t, r.Generation(), committed.Generation)
	assert.Equal(t, 99, committed.ConfigVersion)
	wantDigest, _ := configCanonicalBytesDigest(cand)
	assert.Equal(t, wantDigest, committed.Digest, "artifact records the canonical digest")

	got, err := codec.decode(committed.ConfigBytes)
	require.NoError(t, err, "artifact bytes must decode back to the committed config")
	assert.True(t, configContentEqual(got, f.sup.Config()),
		"the decoded artifact equals the running (committed) config")
}

// TestRolloutApplier_CommitArtifactWriteIsIdempotent proves re-adopting the same
// committed generation does not conflict on the artifact (every member writes it
// at commit; N idempotent writes must not error).
func TestRolloutApplier_CommitArtifactWriteIsIdempotent(t *testing.T) {
	codec := newConfigCodecFake()
	f := newApplierFixtureCodec(t, "node-a", codec)
	r := f.propose(t, liveSafeCandidate(99))
	commitAs(t, f.store, r.Generation(), "node-a", "node-b")

	for range 3 {
		require.NoError(t, f.applier.step(context.Background()))
	}
	committed, err := f.store.CommittedConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, r.Generation(), committed.Generation)
}

// TestSupervisorRun_BootsOnCommittedAfterAbort proves the seq-3 fix is wired into
// Run: a member whose config source holds an aborted candidate starts on the
// durable committed config instead of failing to start — and the runtime it
// builds is the committed generation, not the rejected one.
func TestSupervisorRun_BootsOnCommittedAfterAbort(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	committedCfg := coordinatedClusteredCfg("r1")
	committedCfg.Version = 7
	seedCommittedArtifact(t, store, codec, committedCfg, 3)
	aborted := liveSafeCandidate(8)
	seedRollout(t, store, aborted, persistence.RolloutAborted)

	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = time.Hour
	rc.LeaseTTL = time.Hour
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	s := newTestSupervisor(WithClusterRollout(rc))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx, aborted, nil) }()

	wait.Until(t, 3*time.Second, "member boots on committed config v7", func() bool {
		c := s.Config()
		return c != nil && c.Version == 7
	})
	cancel()
	<-errCh
}

// TestRolloutApplier_ReconcilesToCommittedArtifact is the seq-2 fix: a member
// that missed a commit (its active rollout row moved on before it observed the
// commit) converges to the durable last-committed artifact — fetching the
// committed BYTES, so it needs no staged candidate. Without this it would run an
// older generation than the cohort (a mixed-version window G2 forbids).
func TestRolloutApplier_ReconcilesToCommittedArtifact(t *testing.T) {
	codec := newConfigCodecFake()
	f := newApplierFixtureCodec(t, "node-a", codec)
	require.Equal(t, 0, f.sup.Config().Version, "precondition: running the initial config")

	// A peer committed generation 1 (v99) and wrote the durable artifact; this
	// member never observed that generation and never staged its candidate.
	committed := liveSafeCandidate(99)
	seedCommittedArtifact(t, f.store, codec, committed, 1)

	require.NoError(t, f.applier.step(context.Background()))
	assert.Equal(t, 99, f.sup.Config().Version,
		"the member reconciles to the committed artifact it missed")
}

// TestRolloutApplier_ReconcileIsIdempotent proves reconcile does not rebuild the
// runtime once the member is at the artifact's generation.
func TestRolloutApplier_ReconcileIsIdempotent(t *testing.T) {
	codec := newConfigCodecFake()
	f := newApplierFixtureCodec(t, "node-a", codec)
	committed := liveSafeCandidate(99)
	seedCommittedArtifact(t, f.store, codec, committed, 1)

	require.NoError(t, f.applier.step(context.Background()))
	appliedRt := f.sup.Runtime()
	for range 3 {
		require.NoError(t, f.applier.step(context.Background()))
	}
	assert.Same(t, appliedRt, f.sup.Runtime(), "an already-reconciled generation must not re-swap")
}

// TestRolloutApplier_ReconcileIgnoresBaselineSeed proves the baseline seed
// (generation 0 = the config the member already booted on) never triggers a
// reconcile swap.
func TestRolloutApplier_ReconcileIgnoresBaselineSeed(t *testing.T) {
	codec := newConfigCodecFake()
	f := newApplierFixtureCodec(t, "node-a", codec)
	booted := f.sup.Config()
	// Seed the baseline the member is already running at generation 0.
	seedCommittedArtifact(t, f.store, codec, coordinatedClusteredCfg("r1"), 0)

	rt := f.sup.Runtime()
	require.NoError(t, f.applier.step(context.Background()))
	assert.Same(t, rt, f.sup.Runtime(), "the baseline seed is not a generation to adopt")
	assert.True(t, configContentEqual(f.sup.Config(), booted))
}

// codecJoinerSupervisor builds a NON-running Supervisor with the barrier AND the
// committed-config codec wired, for exercising the boot-config resolution.
func codecJoinerSupervisor(store ports.ClusterRolloutStore, codec *configCodecFake) *Supervisor {
	rc := testRolloutConfig(store, "node-a")
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	return newTestSupervisor(WithClusterRollout(rc))
}

// seedCommittedArtifact writes cfg as the durable committed artifact at gen and
// registers it with the codec so a later decode returns it.
func seedCommittedArtifact(t *testing.T, store ports.ClusterCommittedConfigStore, codec *configCodecFake, cfg *ports.BridgeConfig, gen uint64) {
	t.Helper()
	raw := codec.register(cfg)
	digest, ok := configCanonicalBytesDigest(cfg)
	require.True(t, ok)
	require.NoError(t, store.PutCommittedConfig(context.Background(), persistence.CommittedRolloutConfig{
		Generation: gen, ConfigVersion: cfg.Version, ConfigBytes: raw, Digest: digest,
	}))
}

// TestResolveBoot_FreshCohortBootsWithoutSeeding proves a fresh coordinated boot
// (no artifact yet) boots on its deploy config via the conservative joiner rule
// WITHOUT durably seeding — seeding off `current` is unsound (a candidate in the
// write→propose window is locally indistinguishable from a baseline and would
// poison the artifact). The artifact stays absent until the first real commit.
func TestResolveBoot_FreshCohortBootsWithoutSeeding(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	s := codecJoinerSupervisor(store, codec)
	cfg := coordinatedClusteredCfg("r1")
	cfg.Version = 7

	boot, err := s.resolveCoordinatedBoot(context.Background(), cfg)
	require.NoError(t, err)
	assert.True(t, configContentEqual(boot, cfg), "a fresh cohort boots on its deploy config")

	_, err = store.CommittedConfig(context.Background())
	assert.ErrorIs(t, err, shared.ErrNotFound, "no artifact is seeded off an unverified boot config")
}

// TestResolveBoot_FreshCohortRefusesAbortedCandidate proves the conservative
// fallback still refuses an aborted boot config while no artifact exists (there is
// no committed config to recover to yet).
func TestResolveBoot_FreshCohortRefusesAbortedCandidate(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	cfg := coordinatedClusteredCfg("r1")
	cfg.Version = 7
	seedRollout(t, store, cfg, persistence.RolloutAborted)

	s := codecJoinerSupervisor(store, codec)
	_, err := s.resolveCoordinatedBoot(context.Background(), cfg)
	require.Error(t, err, "with no committed artifact, an aborted boot config is still refused")
	assert.Contains(t, err.Error(), "ABORTED")
}

// TestResolveBoot_StaleReplacementDeltaBootsOnCommitted proves the version gate on
// the whole-cohort-replacement carve-out: a replacement-required delta that is
// OLDER than the committed artifact is a stale / rolled-back boot config, so the
// member boots on the committed config rather than running an old config alone.
func TestResolveBoot_StaleReplacementDeltaBootsOnCommitted(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	committedCfg := coordinatedClusteredCfg("r1")
	committedCfg.Bridge.Cluster.Members = []string{"node-a", "node-b", "node-c"}
	committedCfg.Version = 9
	seedCommittedArtifact(t, store, codec, committedCfg, 4)

	// A stale boot config: older version, and a replacement-required delta (roster
	// shrink) relative to the committed artifact.
	stale := coordinatedClusteredCfg("r1") // members node-a,node-b ; version 8
	stale.Version = 8

	s := codecJoinerSupervisor(store, codec)
	boot, err := s.resolveCoordinatedBoot(context.Background(), stale)
	require.NoError(t, err)
	assert.True(t, configContentEqual(boot, committedCfg),
		"a stale replacement-delta boot config must boot on the committed config, not itself")
	assert.Equal(t, 9, boot.Version)
}

// TestResolveBoot_AllowsTheCommittedConfig proves the ordinary restart: the boot
// config equals the committed artifact, so the node boots on it directly.
func TestResolveBoot_AllowsTheCommittedConfig(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	cfg := coordinatedClusteredCfg("r1")
	cfg.Version = 7
	seedCommittedArtifact(t, store, codec, cfg, 3)

	s := codecJoinerSupervisor(store, codec)
	boot, err := s.resolveCoordinatedBoot(context.Background(), cfg)
	require.NoError(t, err)
	assert.True(t, configContentEqual(boot, cfg))
}

// TestResolveBoot_BootsOnCommittedAfterAbort is the seq-3 fix: a member restarting
// after its rollout ABORTED finds the rejected candidate in its config source, but
// boots on the durable last-committed config instead of refusing to start (which
// is what today's fail-closed joiner does). This is UC-CR3's "rejoin on the old
// committed generation".
func TestResolveBoot_BootsOnCommittedAfterAbort(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	committedCfg := coordinatedClusteredCfg("r1")
	committedCfg.Version = 7
	seedCommittedArtifact(t, store, codec, committedCfg, 3)

	// The config source now holds the aborted candidate (a live-safe delta).
	aborted := liveSafeCandidate(8)
	seedRollout(t, store, aborted, persistence.RolloutAborted)

	s := codecJoinerSupervisor(store, codec)
	boot, err := s.resolveCoordinatedBoot(context.Background(), aborted)

	require.NoError(t, err, "the member must rejoin on the committed config, not refuse to start")
	assert.True(t, configContentEqual(boot, committedCfg),
		"it boots on the last committed config (v7), not the aborted candidate (v8)")
	assert.Equal(t, 7, boot.Version)
}

// TestResolveBoot_BootsOnCommittedInProposeWindow is the seq-1 fix: a member
// restarting after the operator wrote a candidate to the config source but BEFORE
// any rollout proposed it boots on the committed config, not the un-agreed
// candidate — closing the mixed-version window.
func TestResolveBoot_BootsOnCommittedInProposeWindow(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	committedCfg := coordinatedClusteredCfg("r1")
	committedCfg.Version = 7
	seedCommittedArtifact(t, store, codec, committedCfg, 3)

	// No rollout has been proposed for this candidate yet.
	candidate := liveSafeCandidate(8)

	s := codecJoinerSupervisor(store, codec)
	boot, err := s.resolveCoordinatedBoot(context.Background(), candidate)

	require.NoError(t, err)
	assert.True(t, configContentEqual(boot, committedCfg),
		"it boots on the committed config, not the un-proposed candidate")
}

// TestResolveBoot_WholeCohortReplacementBootsOnNewConfig proves the escape hatch
// still works: a replacement-required delta cannot roll through the barrier, so a
// member deployed with it (a whole-cohort replacement) boots on the NEW config,
// not the stale committed artifact.
func TestResolveBoot_WholeCohortReplacementBootsOnNewConfig(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	committedCfg := coordinatedClusteredCfg("r1")
	committedCfg.Version = 7
	seedCommittedArtifact(t, store, codec, committedCfg, 3)

	// A roster change is replacement-required (clusterShapeChanged): the members
	// list IS the barrier's epoch, so it cannot be rolled through the barrier.
	replacement := coordinatedClusteredCfg("r1")
	replacement.Bridge.Cluster.Members = []string{"node-a", "node-b", "node-c"}
	replacement.Version = 8

	s := codecJoinerSupervisor(store, codec)
	boot, err := s.resolveCoordinatedBoot(context.Background(), replacement)

	require.NoError(t, err)
	assert.True(t, configContentEqual(boot, replacement),
		"a whole-cohort replacement boots on the new config")
}

// TestResolveBoot_FailsClosedWhenCommittedUndecodable proves the fail-closed
// posture: if the committed artifact cannot be reconstructed, the member refuses
// to start rather than guess a config and risk splitting the cohort.
func TestResolveBoot_FailsClosedWhenCommittedUndecodable(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	committedCfg := coordinatedClusteredCfg("r1")
	committedCfg.Version = 7
	digest, _ := configCanonicalBytesDigest(committedCfg)
	// Write an artifact whose bytes the codec cannot decode (never registered).
	require.NoError(t, store.PutCommittedConfig(context.Background(), persistence.CommittedRolloutConfig{
		Generation: 3, ConfigVersion: 7, ConfigBytes: []byte("garbage"), Digest: digest,
	}))

	candidate := liveSafeCandidate(8)
	s := codecJoinerSupervisor(store, codec)
	_, err := s.resolveCoordinatedBoot(context.Background(), candidate)
	require.Error(t, err, "an undecodable committed artifact must fail the boot closed")
}

// TestResolveBoot_NilCodecKeepsFailClosedRefusal proves backward compatibility:
// with no codec wired, resolveCoordinatedBoot keeps the conservative joiner rule
// (refuse an aborted boot config) rather than the committed-artifact behavior.
func TestResolveBoot_NilCodecKeepsFailClosedRefusal(t *testing.T) {
	store := memoryrollout.NewStore()
	cfg := coordinatedClusteredCfg("r1")
	cfg.Version = 7
	seedRollout(t, store, cfg, persistence.RolloutAborted)

	s := joinerSupervisor(store) // no codec
	_, err := s.resolveCoordinatedBoot(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ABORTED")
}

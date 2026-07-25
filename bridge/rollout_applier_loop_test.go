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

// applierFixture is a running Supervisor with the rollout barrier wired, plus
// direct handles to the rollout store and a caller-driven applier. The drive
// goroutine is NOT what most of these tests exercise: they call step() explicitly
// so every observation is deterministic and no test depends on a tick landing.
type applierFixture struct {
	sup     *Supervisor
	store   *memoryrollout.Store
	applier *rolloutApplier
	changes chan *ports.BridgeConfig
	swaps   <-chan SwapEvent
}

func newApplierFixture(t *testing.T, memberID string) *applierFixture {
	t.Helper()
	store := memoryrollout.NewStore()
	onSwap, swaps := swapChan(4)
	// Hour-long cadence knobs keep the drive goroutine out of the way: these
	// tests step the applier by hand, and a background tick racing them would
	// make the store state non-deterministic.
	rc := testRolloutConfig(store, memberID)
	rc.PollInterval = time.Hour
	rc.LeaseTTL = time.Hour
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

// propose drives cand through the Supervisor's normal apply path, which is how a
// real member stages a candidate: its config source delivers the change, the
// cluster guard classifies it live-safe, and the proposer stages it alongside
// the Propose. Returns the resulting rollout.
func (f *applierFixture) propose(t *testing.T, cand *ports.BridgeConfig) persistence.Rollout {
	t.Helper()
	require.True(t, sendConfig(f.changes, cand, time.Second))
	ev := awaitSwap(t, f.swaps)
	require.NoError(t, ev.Error, "the candidate must reach the barrier")
	require.True(t, ev.Deferred)
	r, err := f.store.Current(context.Background())
	require.NoError(t, err)
	return r
}

// seedForeignProposal opens a rollout for cand proposed by a PEER, and stages
// cand locally — the shape for testing the applier against a rollout this node
// did not open itself.
func (f *applierFixture) seedForeignProposal(t *testing.T, cand *ports.BridgeConfig, members ...string) persistence.Rollout {
	t.Helper()
	digest, ok := configCanonicalBytesDigest(cand)
	require.True(t, ok)
	f.sup.rollout.stage(digest, cand, cand)
	r, err := f.store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-b", ConfigDigest: digest, ConfigVersion: cand.Version,
		Members: members, TTL: time.Minute,
	})
	require.NoError(t, err)
	return r
}

// liveSafeCandidate returns a coordinated-cluster config differing from the
// running one only in a binding address — a delta the §8 preflight classifies
// live-safe.
func liveSafeCandidate(version int) *ports.BridgeConfig {
	cfg := coordinatedClusteredCfg("r1")
	cfg.Bindings[0].Address = "addr/rolled"
	cfg.Version = version
	return cfg
}

// commitAs stands in for the elected coordinator so applier tests do not also
// exercise election: it acks for the given members and commits under a token.
func commitAs(t *testing.T, store *memoryrollout.Store, gen uint64, members ...string) {
	t.Helper()
	for _, m := range members {
		require.NoError(t, store.Ack(context.Background(), gen, m, "build-digest"))
	}
	require.NoError(t, store.Commit(context.Background(), gen,
		persistence.LeaseToken{Owner: "coord", Version: 1}))
}

// TestRolloutApplier_AcksLiveSafeCandidate proves the vote half of the barrier:
// a member that can verify the digest, classify the delta live-safe, and BUILD
// the candidate records an Ack. The ack is what the coordinator counts against
// the epoch (I2), so without it no rollout can ever commit.
func TestRolloutApplier_AcksLiveSafeCandidate(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	r := f.propose(t, liveSafeCandidate(99))

	require.NoError(t, f.applier.step(context.Background()))

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Contains(t, got.Acks(), "node-a", "a live-safe, buildable candidate must be acked")
	assert.Equal(t, persistence.RolloutStaging, got.State(), "the first ack advances Proposed -> Staging")
	assert.Equal(t, r.ConfigDigest(), got.Acks()["node-a"].BuildDigest,
		"the ack records WHAT was built, so the audit trail is self-describing")
}

// TestRolloutApplier_VotesAtMostOncePerGeneration proves repeated observations
// are idempotent. The drive re-reads the row every tick, so a second vote would
// otherwise be attempted on every poll and rejected by the aggregate (I5) —
// turning a normal cadence into a permanent error stream.
func TestRolloutApplier_VotesAtMostOncePerGeneration(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	f.propose(t, liveSafeCandidate(99))

	for range 3 {
		require.NoError(t, f.applier.step(context.Background()),
			"re-observing an already-voted rollout must be a no-op, not an error")
	}

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Len(t, got.Acks(), 1)
}

// TestRolloutApplier_NacksReplacementRequiredCandidate proves the §8 class gate
// runs on the APPLIER, not only on the proposer. A member whose local view makes
// the delta replacement-required must Nack so the coordinator aborts (F2) — the
// coordinated path can never admit a delta the single-node path would refuse.
func TestRolloutApplier_NacksReplacementRequiredCandidate(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	cand := coordinatedClusteredCfg("r1")
	cand.Stores.Lease = &ports.StoreConfig{Type: "memory"}
	cand.Version = 99
	f.seedForeignProposal(t, cand, "node-a", "node-b")

	require.NoError(t, f.applier.step(context.Background()))

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Contains(t, got.Nacks(), "node-a")
	assert.NotContains(t, got.Acks(), "node-a", "a replacement-required delta must never be acked")
	assert.NotEmpty(t, got.Nacks()["node-a"], "the nack must carry an operator-facing class reason")
}

// TestRolloutApplier_NoStagedCandidateDoesNotVote proves a member whose own
// config source has not delivered the candidate stays SILENT rather than
// guessing. Voting without the candidate would either ack a config it never
// validated (breaking what I2 means) or nack a change that is merely in flight.
// The coordinator's deadline (F1) bounds the wait.
func TestRolloutApplier_NoStagedCandidateDoesNotVote(t *testing.T) {
	f := newApplierFixture(t, "node-a")

	_, err := f.store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-b", ConfigDigest: "a-digest-this-node-has-never-seen", ConfigVersion: 99,
		Members: []string{"node-a", "node-b"}, TTL: time.Minute,
	})
	require.NoError(t, err)

	require.NoError(t, f.applier.step(context.Background()))

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got.Acks())
	assert.Empty(t, got.Nacks())
	assert.Equal(t, persistence.RolloutProposed, got.State())
}

// TestRolloutApplier_NonEpochMemberDoesNotVote proves a node outside the frozen
// epoch never votes. The aggregate would reject it (I5), so attempting would turn
// every observation into an error; more importantly a vote from outside the epoch
// has no meaning for a barrier that counts acks against the epoch.
func TestRolloutApplier_NonEpochMemberDoesNotVote(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	f.seedForeignProposal(t, liveSafeCandidate(99), "node-b", "node-c")

	require.NoError(t, f.applier.step(context.Background()),
		"a node outside the epoch observes without voting")

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got.Acks())
	assert.Empty(t, got.Nacks())
}

// TestRolloutApplier_AdoptsCommittedGeneration proves the commit half: once the
// barrier is Committed this member — and only then — performs its local swap.
// This is the ONLY path by which a coordinated member changes config.
func TestRolloutApplier_AdoptsCommittedGeneration(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	r := f.propose(t, liveSafeCandidate(99))
	oldRt := f.sup.Runtime()
	require.NotNil(t, oldRt)
	require.Equal(t, 0, f.sup.Config().Version, "nothing applied before the commit")

	commitAs(t, f.store, r.Generation(), "node-a", "node-b")
	require.NoError(t, f.applier.step(context.Background()))

	assert.Equal(t, 99, f.sup.Config().Version, "the committed generation is applied locally")
	assert.NotSame(t, oldRt, f.sup.Runtime(), "the swap really happened")
	assert.Equal(t, "addr/rolled", f.sup.Config().Bindings[0].Address)
}

// TestRolloutApplier_AbortedGenerationNeverSwaps proves the abort path: a member
// that acked and built must NOT apply when the coordinator aborts. A member
// swapping on an aborted rollout is precisely the mixed-version cohort G2
// forbids.
func TestRolloutApplier_AbortedGenerationNeverSwaps(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	r := f.propose(t, liveSafeCandidate(99))
	oldRt := f.sup.Runtime()

	require.NoError(t, f.applier.step(context.Background())) // ack
	require.NoError(t, f.store.Abort(context.Background(), r.Generation(),
		persistence.LeaseToken{Owner: "coord", Version: 1}, "a peer nacked"))

	require.NoError(t, f.applier.step(context.Background()))

	assert.Equal(t, 0, f.sup.Config().Version, "an aborted rollout must not be applied")
	assert.Same(t, oldRt, f.sup.Runtime(), "the running runtime is untouched")
}

// TestRolloutApplier_AdoptIsIdempotent proves re-observing a committed rollout
// does not rebuild the runtime. The drive polls, so without this every tick after
// a commit would stop and rebuild the whole runtime — a permanent outage loop
// rather than a reconfiguration.
func TestRolloutApplier_AdoptIsIdempotent(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	r := f.propose(t, liveSafeCandidate(99))
	commitAs(t, f.store, r.Generation(), "node-a", "node-b")

	require.NoError(t, f.applier.step(context.Background()))
	appliedRt := f.sup.Runtime()

	for range 3 {
		require.NoError(t, f.applier.step(context.Background()))
	}
	assert.Same(t, appliedRt, f.sup.Runtime(), "an already-applied generation must not re-swap")
}

// TestRolloutApplier_AdoptIsRestartSafeWithoutADurableGate proves the design's
// nodeRolloutGate-persistence item is discharged by CONTENT rather than by a
// durable counter. A fresh applier (the post-restart state: high-water reset to
// zero) observing an already-applied committed generation must NOT rebuild the
// runtime — it recognises that the running config already IS that generation.
func TestRolloutApplier_AdoptIsRestartSafeWithoutADurableGate(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	r := f.propose(t, liveSafeCandidate(99))
	commitAs(t, f.store, r.Generation(), "node-a", "node-b")
	require.NoError(t, f.applier.step(context.Background()))
	appliedRt := f.sup.Runtime()

	// The restart: a brand-new applier with an empty in-memory gate, against the
	// same durable store and the same running config.
	restarted := &rolloutApplier{sup: f.sup, store: f.store, memberID: "node-a"}
	require.NoError(t, restarted.step(context.Background()))

	assert.Same(t, appliedRt, f.sup.Runtime(),
		"a restarted member must not rebuild a generation it already runs")
}

// TestRolloutApplier_NacksUnbuildableCandidate proves an Ack means "validated
// AND built". A candidate that passes classification but fails the prepare phase
// on THIS member must be nacked, so one member's inability to build the config
// aborts the rollout instead of committing a config it cannot run (F2).
//
// Scope of the proof, deliberately: prepare validates the blueprint, opens the
// durable stores, and assembles runtime options — it does NOT open transport
// sessions, which belong to the commit phase. So an Ack means "this member can
// build it", not "this member reached the broker with it". That is the design's
// Model A (§8.1): commit is not convergence, and a committed config that fails
// to converge is alarmed per node (F8), never rolled back.
func TestRolloutApplier_NacksUnbuildableCandidate(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	cand := coordinatedClusteredCfg("r1")
	cand.Routes[0].ReceiverID = "no-such-receiver"
	cand.Version = 99
	f.seedForeignProposal(t, cand, "node-a", "node-b")

	require.NoError(t, f.applier.step(context.Background()))

	got, err := f.store.Current(context.Background())
	require.NoError(t, err)
	require.Contains(t, got.Nacks(), "node-a")
	assert.Contains(t, got.Nacks()["node-a"], "build failed")
}

// TestRolloutApplier_NoRolloutIsANoOp proves a store with no rollout row at all
// is benign. Every member polls from process start, long before any operator
// change, so ErrNotFound must be silence and not an error stream.
func TestRolloutApplier_NoRolloutIsANoOp(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	require.NoError(t, f.applier.step(context.Background()))
}

// TestRolloutApplier_StoreOutageIsReported proves a real store failure surfaces
// as an error the drive logs and retries (F9), instead of being swallowed into a
// silent stall. Nothing may flip during an outage.
func TestRolloutApplier_StoreOutageIsReported(t *testing.T) {
	f := newApplierFixture(t, "node-a")
	a := &rolloutApplier{
		sup:      f.sup,
		store:    &failingRolloutStore{err: errors.New("dynamodb unavailable")},
		memberID: "node-a",
	}

	require.Error(t, a.step(context.Background()))
	assert.Equal(t, 0, f.sup.Config().Version, "an outage flips nothing")
}

// TestRolloutApplier_DriveAppliesCommittedRollout proves the WIRED drive — not a
// hand-stepped applier — carries a rollout from Committed to a local swap. It is
// the end-to-end proof that Supervisor.Run actually starts the barrier drive.
func TestRolloutApplier_DriveAppliesCommittedRollout(t *testing.T) {
	store := memoryrollout.NewStore()
	onSwap, swaps := swapChan(4)
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = 5 * time.Millisecond
	// An hour-long lease TTL is also the coordinator's lock delay, so this node
	// never decides on its own: the test stands in for the coordinator, keeping
	// the assertion about the APPLIER half only.
	rc.LeaseTTL = time.Hour
	s := newTestSupervisor(WithOnSwap(onSwap), WithClusterRollout(rc))

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), changes)
	defer func() { cancel(); <-errCh }()

	require.True(t, sendConfig(changes, liveSafeCandidate(99), time.Second))
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)
	require.True(t, ev.Deferred)

	r, err := store.Current(context.Background())
	require.NoError(t, err)

	wait.Until(t, 2*time.Second, "the drive acks the candidate on its own", func() bool {
		got, err := store.Current(context.Background())
		return err == nil && len(got.Acks()) == 1
	})
	commitAs(t, store, r.Generation(), "node-b")

	wait.Until(t, 2*time.Second, "the drive applies the committed generation", func() bool {
		return s.Config().Version == 99
	})
}

// failingRolloutStore fails every call with a store error, standing in for a
// DynamoDB outage (F9).
type failingRolloutStore struct{ err error }

func (f *failingRolloutStore) Propose(context.Context, persistence.RolloutProposal) (persistence.Rollout, error) {
	return persistence.Rollout{}, f.err
}
func (f *failingRolloutStore) Ack(context.Context, uint64, string, string) error  { return f.err }
func (f *failingRolloutStore) Nack(context.Context, uint64, string, string) error { return f.err }
func (f *failingRolloutStore) Commit(context.Context, uint64, persistence.LeaseToken) error {
	return f.err
}
func (f *failingRolloutStore) Abort(context.Context, uint64, persistence.LeaseToken, string) error {
	return f.err
}
func (f *failingRolloutStore) Current(context.Context) (persistence.Rollout, error) {
	return persistence.Rollout{}, f.err
}

var _ ports.ClusterRolloutStore = (*failingRolloutStore)(nil)

// TestCandidateConfigDigest_IsStableAcrossIndependentLoads pins the determinism
// the barrier depends on (design §11 Phase 4, previously "UNPROVEN"). Every
// member computes the candidate digest itself from the config its OWN source
// delivered, so two independently-constructed but content-identical configs MUST
// canonicalise to the same digest — otherwise the first proposer wins and every
// peer nacks (F10), aborting every rollout the cohort ever attempts.
func TestCandidateConfigDigest_IsStableAcrossIndependentLoads(t *testing.T) {
	a, aok := configCanonicalBytesDigest(liveSafeCandidate(99))
	b, bok := configCanonicalBytesDigest(liveSafeCandidate(99))
	require.True(t, aok)
	require.True(t, bok)
	assert.Equal(t, a, b, "content-identical configs must produce one digest cohort-wide")

	other, ok := configCanonicalBytesDigest(liveSafeCandidate(100))
	require.True(t, ok)
	assert.NotEqual(t, a, other, "a version bump is a content change and must be visible in the digest")
}

// TestCandidateConfigDigest_SecretValuesParticipate records the DECISION about
// what the digest covers (design §11 Phase 4: "revealed vs referenced secrets").
// It is computed over REVEALED secrets, so two members whose sources disagree on
// a secret VALUE compute different digests and the disagreement aborts the
// rollout instead of splitting the cohort. This is safe against the "every
// rollout nacks" failure the design feared because GoBridge performs NO per-node
// interpolation on the config load path — a shared.Secret is a literal carried in
// the config document, not a reference resolved per process.
func TestCandidateConfigDigest_SecretValuesParticipate(t *testing.T) {
	withKey := func(v string) *ports.BridgeConfig {
		cfg := liveSafeCandidate(99)
		cfg.HTTP = &ports.HTTPConfig{AdminAddr: ":8080", AdminAPIKey: shared.NewSecret(v)}
		return cfg
	}
	a, ok := configCanonicalBytesDigest(withKey("value-a"))
	require.True(t, ok)
	b, ok := configCanonicalBytesDigest(withKey("value-b"))
	require.True(t, ok)

	assert.NotEqual(t, a, b, "a differing secret VALUE is a content difference the barrier must see")
	sameAgain, ok := configCanonicalBytesDigest(withKey("value-a"))
	require.True(t, ok)
	assert.Equal(t, a, sameAgain, "the same secret value must digest identically on every member")
}

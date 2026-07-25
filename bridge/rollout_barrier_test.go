package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSupervisorCoordinatedRollout_LiveSafeDeltaIsProposedToTheBarrier proves the
// Phase-4 guard lift: with the rollout barrier wired, a live-safe delta in a
// coordinated cluster is PROPOSED cluster-wide and deferred locally, instead of
// the Phase-3 fail-closed error.
//
// The two halves that make this safe are asserted together: the delta reaches
// the barrier (a Proposed rollout carrying this config's version and digest),
// and this node does NOT swap — no member may move ahead of the all-member
// commit, which is the mixed-version cohort ADR 0012 exists to prevent.
func TestSupervisorCoordinatedRollout_LiveSafeDeltaIsProposedToTheBarrier(t *testing.T) {
	store := memoryrollout.NewStore()
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(
		WithOnSwap(onSwap),
		WithClusterRollout(ClusterRolloutConfig{
			Store:      store,
			MemberID:   "node-a",
			Membership: func() []string { return []string{"node-a", "node-b"} },
		}),
	)
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	proposed := coordinatedClusteredCfg("r1")
	proposed.Version = 99
	require.True(t, sendConfig(ch, proposed, time.Second))

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error,
		"a live-safe coordinated delta is handed to the barrier, not refused")
	assert.True(t, ev.Deferred, "the delta is committed-not-applied until the barrier commits")
	assert.Equal(t, DeferReasonRolloutPending, ev.DeferReason,
		"the deferral must name the barrier so it is not misreported as an admin pause")

	// The barrier carries the proposal...
	r, err := store.Current(context.Background())
	require.NoError(t, err, "the delta must have been proposed to the rollout store")
	assert.Equal(t, persistence.RolloutProposed, r.State())
	assert.Equal(t, 99, r.ConfigVersion(), "the proposal carries the delta's config version")
	assert.NotEmpty(t, r.ConfigDigest(), "the proposal carries a candidate digest for members to verify")
	assert.Equal(t, []string{"node-a", "node-b"}, r.MembershipEpoch(),
		"the epoch is frozen at Propose from the live membership")

	// ...and NOTHING swapped locally.
	assert.Same(t, oldRt, s.Runtime(), "no member may swap before the all-member commit")
	assert.Equal(t, 0, s.Config().Version, "the applied config version must not advance on Propose")
}

// seedForeignRollout opens a rollout on store for a candidate config that is NOT
// the one under test, so a subsequent local Propose collides with it.
func seedForeignRollout(t *testing.T, store *memoryrollout.Store, members ...string) {
	t.Helper()
	_, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID:    "node-b",
		ConfigDigest:  candidateConfigDigest([]byte("a completely different candidate config")),
		ConfigVersion: 7,
		Members:       members,
		TTL:           time.Minute,
	})
	require.NoError(t, err)
}

// TestSupervisorCoordinatedRollout_ForeignActiveRolloutFailsClosed proves the
// proposer does not mistake ANY active rollout for "a peer already proposed MY
// delta". Propose returns shared.ErrAlreadyExists whenever a rollout is active
// (invariant I1) — including one carrying a DIFFERENT candidate config. Treating
// that as success would defer this node's delta against a barrier that will
// commit somebody else's config: the delta is reported committed-not-applied to
// the admin API (ports.ErrApplyInFlight → no rollback) and then never applies by
// anyone. That is the silent-drop the Supervisor's own guard comment forbids, so
// the collision MUST fail closed with a visible error and no deferral.
func TestSupervisorCoordinatedRollout_ForeignActiveRolloutFailsClosed(t *testing.T) {
	store := memoryrollout.NewStore()
	seedForeignRollout(t, store, "node-a", "node-b")

	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(
		WithOnSwap(onSwap),
		WithClusterRollout(ClusterRolloutConfig{
			Store:      store,
			MemberID:   "node-a",
			Membership: func() []string { return []string{"node-a", "node-b"} },
		}),
	)
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	proposed := coordinatedClusteredCfg("r1")
	proposed.Version = 99
	require.True(t, sendConfig(ch, proposed, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error,
		"a collision with a FOREIGN active rollout must fail closed, not be reported as deferred")
	assert.False(t, ev.Deferred,
		"an unproposed delta must never be reported as committed-not-applied")
	assert.Same(t, oldRt, s.Runtime(), "the running config keeps serving")

	// The foreign rollout is untouched: this node did not join or corrupt it.
	r, err := store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 7, r.ConfigVersion(), "the peer's rollout must not be re-stamped")
}

// TestSupervisorCoordinatedRollout_PeerProposedSameDeltaJoins proves the
// converse: when the active rollout carries THIS node's exact candidate digest,
// the peer simply won the conditional create and this node joins that generation
// — a deferral, not an error. Every member proposes the same delta, so exactly
// one Propose wins and the rest must not turn the race into a refusal.
func TestSupervisorCoordinatedRollout_PeerProposedSameDeltaJoins(t *testing.T) {
	store := memoryrollout.NewStore()
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(
		WithOnSwap(onSwap),
		WithClusterRollout(ClusterRolloutConfig{
			Store:      store,
			MemberID:   "node-a",
			Membership: func() []string { return []string{"node-a", "node-b"} },
		}),
	)
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()

	proposed := coordinatedClusteredCfg("r1")
	proposed.Version = 99

	// A peer proposes the IDENTICAL candidate first (same canonical bytes).
	raw, ok := configCanonicalBytes(proposed)
	require.True(t, ok)
	_, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID:    "node-b",
		ConfigDigest:  candidateConfigDigest(raw),
		ConfigVersion: proposed.Version,
		Members:       []string{"node-a", "node-b"},
		TTL:           time.Minute,
	})
	require.NoError(t, err)

	require.True(t, sendConfig(ch, proposed, time.Second))

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error, "joining the peer's generation for the SAME delta is success")
	assert.True(t, ev.Deferred)
	assert.Equal(t, DeferReasonRolloutPending, ev.DeferReason)
}

// TestSupervisorCoordinatedRollout_SelfExcludedMemberFailsClosed proves a node
// whose MemberID is absent from the membership epoch refuses instead of
// proposing. Such a node can never Ack (persistence.Rollout rejects a voter
// outside the frozen epoch), so the barrier it opened could only ever
// deadline-abort — a guaranteed-dead rollout that blocks every other proposal
// for its whole TTL (invariant I1 allows one active rollout).
func TestSupervisorCoordinatedRollout_SelfExcludedMemberFailsClosed(t *testing.T) {
	store := memoryrollout.NewStore()
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(
		WithOnSwap(onSwap),
		WithClusterRollout(ClusterRolloutConfig{
			Store:      store,
			MemberID:   "node-z", // not in the cohort below
			Membership: func() []string { return []string{"node-a", "node-b"} },
		}),
	)
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()

	proposed := coordinatedClusteredCfg("r1")
	proposed.Version = 99
	require.True(t, sendConfig(ch, proposed, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error, "a self-excluded member must refuse, not open a dead rollout")
	assert.False(t, ev.Deferred)
	_, err := store.Current(context.Background())
	assert.Error(t, err, "nothing may be proposed when this node is outside the epoch")
}

// TestSupervisorCoordinatedRollout_PeerEpochExcludesThisNodeFailsClosed proves
// the join path applies the same self-exclusion rule as the propose path. A peer
// may win the conditional create for the SAME candidate digest yet freeze the
// epoch from a roster that omits this node (the cohort disagrees about
// bridge.cluster.endpoints). Joining it would defer this node's delta behind a
// barrier it can never Ack — guaranteed to deadline-abort — so it fails closed.
func TestSupervisorCoordinatedRollout_PeerEpochExcludesThisNodeFailsClosed(t *testing.T) {
	store := memoryrollout.NewStore()
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(
		WithOnSwap(onSwap),
		WithClusterRollout(ClusterRolloutConfig{
			Store:      store,
			MemberID:   "node-a",
			Membership: func() []string { return []string{"node-a", "node-b"} },
		}),
	)
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()

	proposed := coordinatedClusteredCfg("r1")
	proposed.Version = 99
	raw, ok := configCanonicalBytes(proposed)
	require.True(t, ok)

	// The peer proposes the IDENTICAL candidate, but under a roster without us.
	_, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID:    "node-b",
		ConfigDigest:  candidateConfigDigest(raw),
		ConfigVersion: proposed.Version,
		Members:       []string{"node-b", "node-c"},
		TTL:           time.Minute,
	})
	require.NoError(t, err)

	require.True(t, sendConfig(ch, proposed, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error, "a node outside the peer's epoch must not join a barrier it cannot Ack")
	assert.False(t, ev.Deferred)
}

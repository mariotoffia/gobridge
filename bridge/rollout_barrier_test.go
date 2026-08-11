package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
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
		WithClusterRollout(testRolloutConfig(store, "node-a")),
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

// TestRolloutMembers_RosterIsClusterMembersNotEndpoints pins the membership
// authority. bridge.cluster.endpoints is THIS instance's CAPABILITY map, keyed by
// capability name and validated to carry an "http" key (config.validateClusterEndpoints)
// — it is NOT a peer roster. Reading its keys as member ids would freeze every
// cohort's epoch as ["http"]: a one-member barrier that commits on a single ack,
// i.e. no barrier at all, which is the entire invariant this protocol exists
// to provide. The roster therefore comes from bridge.cluster.members only.
func TestRolloutMembers_RosterIsClusterMembersNotEndpoints(t *testing.T) {
	cfg := supervisorTestConfig("r1")
	cfg.Bridge.Cluster = &ports.ClusterConfig{
		Endpoints: map[string]string{"http": "http://10.0.0.1:8080"},
		Members:   []string{"node-a", "node-b", "node-c"},
	}

	assert.Equal(t, []string{"node-a", "node-b", "node-c"}, rolloutMembers(cfg))
	assert.NotContains(t, rolloutMembers(cfg), "http",
		"an endpoint CAPABILITY key must never be mistaken for a cohort member id")
}

// TestRolloutMembers_AbsentRosterIsUnknown proves an absent roster resolves to
// nil rather than an empty-but-present epoch: the caller must fail closed, never
// propose against a guessed cohort.
func TestRolloutMembers_AbsentRosterIsUnknown(t *testing.T) {
	assert.Nil(t, rolloutMembers(nil))

	cfg := supervisorTestConfig("r1")
	assert.Nil(t, rolloutMembers(cfg), "no cluster block at all")

	cfg.Bridge.Cluster = &ports.ClusterConfig{Endpoints: map[string]string{"http": "http://h:1"}}
	assert.Empty(t, rolloutMembers(cfg), "endpoints alone declare no roster")
}

// TestSupervisorCoordinatedRollout_EmptyRosterFailsClosed proves the PROPOSER's
// own defence against a vacuous epoch, independent of the startup gate that
// normally catches it first. Freezing an empty epoch would make
// Rollout.CanCommit trivially true — every member's ack vacuously covered — so
// the first coordinator observation would commit a config no member validated.
//
// Driven directly rather than through a running Supervisor because a coordinated
// deployment with no roster cannot start at all (see
// TestSupervisorRun_RefusesAnEmptyRoster); this pins the second line of defence
// for a roster that empties out AFTER boot.
func TestSupervisorCoordinatedRollout_EmptyRosterFailsClosed(t *testing.T) {
	store := memoryrollout.NewStore()
	s := newTestSupervisor(WithClusterRollout(testRolloutConfig(store, "node-a")))

	applied := coordinatedClusteredCfg("r1")
	applied.Bridge.Cluster.Members = nil
	proposed := coordinatedClusteredCfg("r1")
	proposed.Bridge.Cluster.Members = nil
	proposed.Version = 99

	err := s.proposeCoordinatedRollout(context.Background(), applied, proposed, proposed)

	require.Error(t, err, "an unknown cohort roster must refuse, not propose a vacuous epoch")
	assert.Contains(t, err.Error(), "bridge.cluster.members")
	_, storeErr := store.Current(context.Background())
	assert.Error(t, storeErr, "nothing may be proposed without a roster")
}

// TestSupervisorRun_RefusesAnEmptyRoster proves the startup gate: a coordinated
// deployment whose roster is missing never starts. Every reload would be refused
// anyway, so failing at boot turns a 3am discovery into a deploy-time one.
func TestSupervisorRun_RefusesAnEmptyRoster(t *testing.T) {
	s := newTestSupervisor(WithClusterRollout(testRolloutConfig(memoryrollout.NewStore(), "node-a")))
	cfg := coordinatedClusteredCfg("r1")
	cfg.Bridge.Cluster.Members = nil

	err := s.Run(t.Context(), cfg, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge.cluster.members")
	assert.Nil(t, s.Runtime())
}

// TestSupervisorRun_RefusesAMemberIdOutsideTheRoster proves the startup gate
// catches the wiring mistake the blueprint validator cannot see: the member id
// is composition-root wiring, not config. A node outside its own roster can
// never Ack, so every rollout it proposes is guaranteed to deadline-abort while
// blocking the cohort's next change for the whole TTL.
func TestSupervisorRun_RefusesAMemberIdOutsideTheRoster(t *testing.T) {
	s := newTestSupervisor(WithClusterRollout(testRolloutConfig(memoryrollout.NewStore(), "node-z")))

	err := s.Run(t.Context(), coordinatedClusteredCfg("r1"), nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "node-z")
	assert.Nil(t, s.Runtime())
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
// including one carrying a DIFFERENT candidate config. Treating
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
		WithClusterRollout(testRolloutConfig(store, "node-a")),
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
		WithClusterRollout(testRolloutConfig(store, "node-a")),
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
// for its whole TTL (invariant allows one active rollout).
//
// Driven directly: the startup gate refuses this wiring outright
// (TestSupervisorRun_RefusesAMemberIdOutsideTheRoster), so this pins the
// proposer's own defence for a roster edited after boot.
func TestSupervisorCoordinatedRollout_SelfExcludedMemberFailsClosed(t *testing.T) {
	store := memoryrollout.NewStore()
	// node-z is not in coordinatedClusteredCfg's roster.
	s := newTestSupervisor(WithClusterRollout(testRolloutConfig(store, "node-z")))

	proposed := coordinatedClusteredCfg("r1")
	proposed.Version = 99

	err := s.proposeCoordinatedRollout(context.Background(), coordinatedClusteredCfg("r1"), proposed, proposed)

	require.Error(t, err, "a self-excluded member must refuse, not open a dead rollout")
	_, storeErr := store.Current(context.Background())
	assert.Error(t, storeErr, "nothing may be proposed when this node is outside the epoch")
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
		WithClusterRollout(testRolloutConfig(store, "node-a")),
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

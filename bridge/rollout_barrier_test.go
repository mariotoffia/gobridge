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

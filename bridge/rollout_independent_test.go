package bridge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// The third way a clustered deployment can treat a live config change.
//
// Two already existed, and neither is what most operators want. The default
// REFUSES a live reload outright: a clustered bridge must be stopped, redeployed
// and started again for any change at all. The coordinated barrier is the other
// extreme: every member builds the candidate first and nobody swaps until all of
// them have, which is safe and needs a shared store, a lease-elected coordinator
// and a cohort roster to run at all.
//
// `independent` is the middle: the change is validated where it is written and
// then applied by each member on its own, exactly as a single bridge applies one.
// A member that cannot run it is a broken member, not a veto. The cost is the
// window in which one member has swapped and another has not, and taking that
// cost is the operator's decision — which is why it is a config value rather than
// a default.
//
// What it does NOT relax is the per-member rule that some changes cannot be
// applied live AT ALL — a durable session's identity, a store's target. Those are
// refused on a single node too, and they stay refused here, with their reason.

func independentClusterConfig(logLevel string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "cohort",
			DeploymentMode: "clustered",
			LogLevel:       logLevel,
			Cluster:        &ports.ClusterConfig{Rollout: "independent"},
		},
	}
}

// TestClassifyClusterReload_IndependentAppliesLiveSafeDeltaDirectly is the whole
// point of the mode: no barrier, no vote, no store — the member applies it the
// way a standalone bridge does.
func TestClassifyClusterReload_IndependentAppliesLiveSafeDeltaDirectly(t *testing.T) {
	disposition, reason := classifyClusterReload(
		independentClusterConfig("info"), independentClusterConfig("debug"))

	require.Equal(t, clusterReloadProceed, disposition,
		"an independent cohort applies a live-safe change directly; reason: %s", reason)
	require.Empty(t, reason)
}

// TestClassifyClusterReload_IndependentStillRefusesWhatCannotBeAppliedLive pins
// the limit. These deltas are refused on a single node too, because a durable
// identity or a store target cannot change under a running session — nothing
// about the cluster mode makes them safe.
func TestClassifyClusterReload_IndependentStillRefusesWhatCannotBeAppliedLive(t *testing.T) {
	before := independentClusterConfig("info")
	before.Stores.Lease = &ports.StoreConfig{Type: "memory"}
	after := independentClusterConfig("info")
	after.Stores.Lease = &ports.StoreConfig{Type: "sqlite"}

	disposition, reason := classifyClusterReload(before, after)

	require.Equal(t, clusterReloadRefuse, disposition)
	require.NotEmpty(t, reason, "the refusal must name the class, as it does on a single node")
}

// TestClassifyClusterReload_ModeMustAgreeOnBothSides keeps the existing
// fail-closed rule. A change that switches the cohort between modes is a change
// to the cohort's own definition and cannot be carried by either of them.
func TestClassifyClusterReload_ModeMustAgreeOnBothSides(t *testing.T) {
	independent := independentClusterConfig("info")
	coordinated := independentClusterConfig("info")
	coordinated.Bridge.Cluster.Rollout = "coordinated"
	coordinated.Bridge.Cluster.Members = []string{"a", "b"}

	for name, delta := range map[string][2]*ports.BridgeConfig{
		"independent to coordinated": {independent, coordinated},
		"coordinated to independent": {coordinated, independent},
	} {
		t.Run(name, func(t *testing.T) {
			disposition, _ := classifyClusterReload(delta[0], delta[1])
			require.Equal(t, clusterReloadRefuse, disposition)
		})
	}
}

// TestIndependentRollout_WiresNoBarrier is the operational consequence an
// operator most needs: choosing this mode means the deployment provisions no
// rollout store, no coordinator lease and no roster, because nothing consults
// them.
func TestIndependentRollout_WiresNoBarrier(t *testing.T) {
	require.False(t, IsCoordinatedRollout(independentClusterConfig("info")),
		"an independent cohort must not wire the coordinated rollout barrier")
}

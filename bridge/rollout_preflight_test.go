package bridge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestClassifyRolloutDelta_LiveSafe_BenignChange validates that a delta which
// changes neither durable session identity, store target, nor lease ownership
// (here: a publish topic/address) is classified live-safe — eligible for a
// coordinated cluster rollout.
func TestClassifyRolloutDelta_LiveSafe_BenignChange(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg.Bindings[0].Address = "topic/changed"

	class, reason := classifyRolloutDelta(oldCfg, newCfg)

	require.Equal(t, rolloutLiveSafe, class, "benign address change must be live-safe (reason=%q)", reason)
	require.Empty(t, reason)
}

// TestClassifyRolloutDelta_ReplacementRequired_MemberRosterChange validates that
// changing bridge.cluster.members is replacement-required. The roster IS the
// membership epoch the barrier freezes, so carrying a roster change through the
// barrier would commit under the OLD roster's acks and leave the cohort running
// a config declaring a DIFFERENT one (design §8, F6).
func TestClassifyRolloutDelta_ReplacementRequired_MemberRosterChange(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	oldCfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"a", "b"}}
	newCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"a", "b", "c"}}

	class, reason := classifyRolloutDelta(oldCfg, newCfg)

	require.Equal(t, rolloutReplacementRequired, class)
	require.Contains(t, reason, "bridge.cluster.members")
}

// TestClassifyRolloutDelta_LiveSafe_MemberRosterReorder validates the roster is
// compared as the SET it is. A reorder (or a repeated id) names the same cohort,
// and the frozen epoch is stored sorted+deduped, so it must NOT be misread as a
// membership change that forces whole-cohort replacement for a no-op edit.
func TestClassifyRolloutDelta_LiveSafe_MemberRosterReorder(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	oldCfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"a", "b", "c"}}
	newCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"c", "a", "b", "a"}}

	class, reason := classifyRolloutDelta(oldCfg, newCfg)

	require.Equal(t, rolloutLiveSafe, class, "same cohort, different spelling (reason=%q)", reason)
}

// TestClassifyRolloutDelta_ReplacementRequired_StoreTargetChange validates that
// changing a durable store's target (lease store type) is replacement-required:
// the old store's backlog would be stranded, so ADR 0012 whole-cohort
// replacement still governs and the coordinated path must refuse.
func TestClassifyRolloutDelta_ReplacementRequired_StoreTargetChange(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg.Stores.Lease = &ports.StoreConfig{Type: "dynamodb"}

	class, reason := classifyRolloutDelta(oldCfg, newCfg)

	require.Equal(t, rolloutReplacementRequired, class)
	require.NotEmpty(t, reason, "replacement-required must carry an operator-facing reason")
}

// TestClassifyRolloutDelta_ReplacementRequired_LeaseSessionIdentity validates
// that changing an exclusive route's lease session_id (the ownership key) is
// replacement-required — a rolling change would split ownership domains.
func TestClassifyRolloutDelta_ReplacementRequired_LeaseSessionIdentity(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess-a")
	newCfg := supervisorTestConfigWithSession("r1", "sess-b")

	class, reason := classifyRolloutDelta(oldCfg, newCfg)

	require.Equal(t, rolloutReplacementRequired, class)
	require.NotEmpty(t, reason)
}

// TestClassifyRolloutDelta_ReplacementRequired_DeploymentModeChange validates
// that a deployment-mode change is replacement-required (design §8: "no
// deployment-mode change") — a topology transition, not a live-safe delta.
func TestClassifyRolloutDelta_ReplacementRequired_DeploymentModeChange(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	oldCfg.Bridge.DeploymentMode = "clustered"
	newCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg.Bridge.DeploymentMode = "standalone"

	class, reason := classifyRolloutDelta(oldCfg, newCfg)

	require.Equal(t, rolloutReplacementRequired, class)
	require.NotEmpty(t, reason)
}

// TestClassifyRolloutDelta_ReplacementRequired_DurableStoreRemoval validates
// that removing a durable store (here the outbox, while a shared_outbox route
// still needs it) is replacement-required — its backlog would be stranded.
// storeIdentityChanged skips a store present in only one config, so this is
// caught by the destructive-shape predicate.
func TestClassifyRolloutDelta_ReplacementRequired_DurableStoreRemoval(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg.Stores.Outbox = nil

	class, reason := classifyRolloutDelta(oldCfg, newCfg)

	require.Equal(t, rolloutReplacementRequired, class)
	require.NotEmpty(t, reason)
}

// TestCoordinatedRollout_EnabledWhenClusteredAndCoordinated validates that the
// guard-lift predicate is true only when the deployment is clustered AND the
// operator has explicitly opted into cluster.rollout: coordinated.
func TestCoordinatedRollout_EnabledWhenClusteredAndCoordinated(t *testing.T) {
	cfg := supervisorTestConfigWithSession("r1", "sess")
	cfg.Bridge.DeploymentMode = "clustered"
	cfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated"}

	require.True(t, coordinatedRollout(cfg))
}

// TestCoordinatedRollout_DisabledByDefault validates that a clustered
// deployment WITHOUT the opt-in stays on the legacy refuse-live-reconfig path
// (ADR 0012) — coordinated rollout must never activate implicitly.
func TestCoordinatedRollout_DisabledByDefault(t *testing.T) {
	cfg := supervisorTestConfigWithSession("r1", "sess")
	cfg.Bridge.DeploymentMode = "clustered"
	cfg.Bridge.Cluster = &ports.ClusterConfig{} // rollout unset

	require.False(t, coordinatedRollout(cfg))
}

// TestCoordinatedRollout_DisabledWhenNotClustered validates that the opt-in is
// inert outside a clustered deployment — there is no cohort to coordinate.
func TestCoordinatedRollout_DisabledWhenNotClustered(t *testing.T) {
	cfg := supervisorTestConfigWithSession("r1", "sess")
	cfg.Bridge.DeploymentMode = "standalone"
	cfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated"}

	require.False(t, coordinatedRollout(cfg))
}

// coordinatedClusterCfg builds a clustered, coordinated-rollout config for the
// given route/session — the steady state in which live-safe deltas are eligible.
func coordinatedClusterCfg(route, session string) *ports.BridgeConfig {
	cfg := supervisorTestConfigWithSession(route, session)
	cfg.Bridge.DeploymentMode = "clustered"
	cfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated"}
	return cfg
}

// TestClassifyClusterReload_ProceedWhenNotClustered validates that a non-
// clustered reload is unaffected by the guard — it proceeds to normal apply.
func TestClassifyClusterReload_ProceedWhenNotClustered(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg.Bindings[0].Address = "topic/changed"

	disp, _ := classifyClusterReload(oldCfg, newCfg)

	require.Equal(t, clusterReloadProceed, disp)
}

// TestClassifyClusterReload_RefuseWhenClusteredNotCoordinated validates the ADR
// 0012 default: a clustered deployment WITHOUT cluster.rollout: coordinated
// still refuses every live reload, fail-closed.
func TestClassifyClusterReload_RefuseWhenClusteredNotCoordinated(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	oldCfg.Bridge.DeploymentMode = "clustered"
	newCfg := supervisorTestConfigWithSession("r2", "sess")
	newCfg.Bridge.DeploymentMode = "clustered"

	disp, _ := classifyClusterReload(oldCfg, newCfg)

	require.Equal(t, clusterReloadRefuse, disp)
}

// TestClassifyClusterReload_CoordinatedWhenLiveSafe validates the lift: within a
// coordinated cluster, a live-safe delta is routed to the coordinated barrier
// rather than refused.
func TestClassifyClusterReload_CoordinatedWhenLiveSafe(t *testing.T) {
	oldCfg := coordinatedClusterCfg("r1", "sess")
	newCfg := coordinatedClusterCfg("r1", "sess")
	newCfg.Bindings[0].Address = "topic/changed" // benign, live-safe

	disp, reason := classifyClusterReload(oldCfg, newCfg)

	require.Equal(t, clusterReloadCoordinated, disp)
	require.Empty(t, reason)
}

// TestClassifyClusterReload_RefuseReplacementRequiredWithReason validates that a
// coordinated cluster still refuses a replacement-required delta, carrying the
// class reason so the operator is pointed at the whole-cohort procedure.
func TestClassifyClusterReload_RefuseReplacementRequiredWithReason(t *testing.T) {
	oldCfg := coordinatedClusterCfg("r1", "sess")
	newCfg := coordinatedClusterCfg("r1", "sess")
	newCfg.Stores.Lease = &ports.StoreConfig{Type: "dynamodb"} // store target change

	disp, reason := classifyClusterReload(oldCfg, newCfg)

	require.Equal(t, clusterReloadRefuse, disp)
	require.NotEmpty(t, reason, "a replacement-required refusal must carry the class reason")
}

// TestClassifyClusterReload_RefuseLeavingCoordinatedCluster validates that
// leaving the cohort (new config not clustered) is refused — the new side is not
// coordinated, so the barrier cannot govern it.
func TestClassifyClusterReload_RefuseLeavingCoordinatedCluster(t *testing.T) {
	oldCfg := coordinatedClusterCfg("r1", "sess")
	newCfg := supervisorTestConfigWithSession("r1", "sess") // standalone

	disp, _ := classifyClusterReload(oldCfg, newCfg)

	require.Equal(t, clusterReloadRefuse, disp)
}

// TestClassifyRolloutDelta_ReplacementRequired_ClusterEndpointsChange proves a
// change to bridge.cluster.endpoints is replacement-required.
//
// NOTE the reason, which is NOT membership: cluster.endpoints is THIS instance's
// advertised CAPABILITY map (keyed by capability, e.g. http), not a peer roster —
// the cohort roster is bridge.cluster.members, covered separately above. The HTTP
// forwarder resolves remote exclusive requests through this map, so changing it
// under live traffic retargets in-flight forwards mid-rollout. An earlier version
// of this test described the key as the roster; that misreading is precisely what
// made the barrier freeze a one-"member" epoch named "http".
func TestClassifyRolloutDelta_ReplacementRequired_ClusterEndpointsChange(t *testing.T) {
	for name, mutate := range map[string]func(*ports.ClusterConfig){
		"capability added":      func(c *ports.ClusterConfig) { c.Endpoints["grpc"] = "10.0.0.3:8080" },
		"capability removed":    func(c *ports.ClusterConfig) { delete(c.Endpoints, "http") },
		"capability retargeted": func(c *ports.ClusterConfig) { c.Endpoints["http"] = "10.0.0.9:8080" },
	} {
		t.Run(name, func(t *testing.T) {
			oldCfg := supervisorTestConfigWithSession("r1", "sess")
			oldCfg.Bridge.Cluster = &ports.ClusterConfig{
				Rollout:   rolloutModeCoordinated,
				Endpoints: map[string]string{"http": "10.0.0.1:8080"},
			}
			newCfg := supervisorTestConfigWithSession("r1", "sess")
			newCfg.Bridge.Cluster = &ports.ClusterConfig{
				Rollout:   rolloutModeCoordinated,
				Endpoints: map[string]string{"http": "10.0.0.1:8080"},
			}
			mutate(newCfg.Bridge.Cluster)

			class, reason := classifyRolloutDelta(oldCfg, newCfg)

			require.Equal(t, rolloutReplacementRequired, class)
			require.NotEmpty(t, reason, "replacement-required must carry an operator-facing reason")
		})
	}
}

// TestClassifyRolloutDelta_ReplacementRequired_RolloutModeChange proves that
// turning the coordinated barrier itself on or off is replacement-required. Both
// sides of classifyClusterReload must already be coordinated for the delta to
// reach here at all, so the only way this fires is a cohort mid-flip — which
// would leave some members driving a barrier the others ignore.
func TestClassifyRolloutDelta_ReplacementRequired_RolloutModeChange(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	oldCfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: rolloutModeCoordinated}
	newCfg := supervisorTestConfigWithSession("r1", "sess")
	newCfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: ""}

	class, reason := classifyRolloutDelta(oldCfg, newCfg)

	require.Equal(t, rolloutReplacementRequired, class)
	require.NotEmpty(t, reason)
}

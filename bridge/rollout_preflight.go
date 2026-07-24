package bridge

import (
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
)

// rolloutDeltaClass classifies a config delta for coordinated-rollout eligibility
// (design §8). It exists so the coordinated cluster-rollout path can never admit
// a change the single-node reload path would reject.
type rolloutDeltaClass int

const (
	// rolloutLiveSafe marks a delta that changes no durable session identity,
	// store target, or lease ownership key — eligible for coordinated rollout.
	rolloutLiveSafe rolloutDeltaClass = iota
	// rolloutReplacementRequired marks a delta that alters durable identity or
	// store targets (the reasons ADR 0012 exists). It is refused live and keeps
	// the whole-cohort replacement procedure (§2 N2).
	rolloutReplacementRequired
)

// rolloutModeCoordinated is the cluster.rollout value that opts a clustered
// deployment into the coordinated rollout barrier (design §8). Any other value
// — including the empty default — keeps the legacy refuse-live-reconfig path.
const rolloutModeCoordinated = "coordinated"

// coordinatedRollout reports whether cfg opts into coordinated cluster rollout:
// the deployment must be clustered AND cluster.rollout must be "coordinated".
// It is the single gate the guard-lift consults, so the coordinated path stays
// off unless an operator has explicitly enabled it on a real cluster.
func coordinatedRollout(cfg *ports.BridgeConfig) bool {
	if !ports.IsClusteredDeployment(cfg) {
		return false
	}
	return cfg.Bridge.Cluster != nil && cfg.Bridge.Cluster.Rollout == rolloutModeCoordinated
}

// clusterReloadDisposition is what the apply-path cluster guard should do with a
// reload, once no-op detection has run.
type clusterReloadDisposition int

const (
	// clusterReloadProceed: neither side is clustered — a normal single-node
	// reload, unaffected by the cluster guard.
	clusterReloadProceed clusterReloadDisposition = iota
	// clusterReloadRefuse: fail closed (ADR 0012). Either the deployment is
	// clustered without cluster.rollout: coordinated, or it is coordinated but
	// the delta is replacement-required (the reason is then non-empty).
	clusterReloadRefuse
	// clusterReloadCoordinated: a live-safe delta within a coordinated cluster —
	// route it into the coordinated rollout barrier instead of refusing.
	clusterReloadCoordinated
)

// classifyClusterReload decides how the apply-path guard should treat a reload
// from oldCfg to newCfg (the lift of §6). The default stays fail-closed: a
// clustered deployment refuses live reloads unless BOTH the applied and proposed
// configs opt into cluster.rollout: coordinated. Only then is the delta
// classified; a live-safe delta is routed to the barrier, a replacement-required
// one is refused with its class reason (pointing at the whole-cohort procedure).
func classifyClusterReload(oldCfg, newCfg *ports.BridgeConfig) (clusterReloadDisposition, string) {
	if !ports.IsClusteredDeployment(oldCfg) && !ports.IsClusteredDeployment(newCfg) {
		return clusterReloadProceed, ""
	}
	// Both sides must be coordinated-clustered; entering, leaving, or a
	// non-coordinated cluster all fall through to the ADR 0012 refusal.
	if !coordinatedRollout(oldCfg) || !coordinatedRollout(newCfg) {
		return clusterReloadRefuse, ""
	}
	if class, reason := classifyRolloutDelta(oldCfg, newCfg); class == rolloutReplacementRequired {
		return clusterReloadRefuse, reason
	}
	return clusterReloadCoordinated, ""
}

// classifyRolloutDelta decides whether the delta from oldCfg to newCfg is
// live-safe (eligible for a coordinated cluster rollout) or replacement-required.
// It reuses the EXACT per-node reload-preflight predicates that already refuse a
// single-node live reload for identity/store-target changes, so the coordinated
// path admits only deltas the single-node path would also accept. The second
// return is a non-empty, operator-facing reason iff replacement-required.
func classifyRolloutDelta(oldCfg, newCfg *ports.BridgeConfig) (rolloutDeltaClass, string) {
	// Fail closed: a delta we cannot fully classify is never admitted live.
	if oldCfg == nil || newCfg == nil {
		return rolloutReplacementRequired, "incomplete config delta; cannot classify"
	}
	for _, check := range []func(_, _ *ports.BridgeConfig) error{
		durableSessionIdentityChanged,
		storeIdentityChanged,
		leaseSessionIDChanged,
	} {
		if err := check(oldCfg, newCfg); err != nil {
			return rolloutReplacementRequired, err.Error()
		}
	}
	// A deployment-mode change is a topology transition, not a live-safe delta
	// (design §8). The store predicates above do not see it.
	if oldCfg.Bridge.DeploymentMode != newCfg.Bridge.DeploymentMode {
		return rolloutReplacementRequired, fmt.Sprintf(
			"deployment_mode change %q -> %q is a topology transition",
			oldCfg.Bridge.DeploymentMode, newCfg.Bridge.DeploymentMode,
		)
	}
	// Durable-store removal / orphaned outbox partitions: storeIdentityChanged
	// skips a store present in only one config, so this catches the removal edge.
	if destructiveReloadShape(oldCfg, newCfg) {
		return rolloutReplacementRequired, "durable store removal or orphaned outbox partition"
	}
	return rolloutLiveSafe, ""
}

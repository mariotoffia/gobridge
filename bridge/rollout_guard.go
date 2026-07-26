package bridge

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
)

// The apply-path cluster guard (design cluster-config-rollout-protocol.md §6),
// extracted from Supervisor.apply so the swap path stays readable and the
// guard's three outcomes — proceed, refuse, hand to the barrier — are testable
// as one unit.
//
// The ADR 0012 refusal stays the DEFAULT: a clustered deployment refuses live
// reloads fail-closed, because a per-process reload has no cluster-wide version
// barrier or coordinated rollback. It is lifted ONLY when BOTH the applied and
// proposed configs opt into cluster.rollout: coordinated AND the delta is
// live-safe (§8); a replacement-required delta is still refused, now with its
// class reason. WithAllowDestructiveReload never applies to either — it discards
// LOCAL durable backlog and cannot substitute for cluster consensus.

// clusterReloadGuard runs the cluster guard for a reload from the applied config
// to frozenCfg. It reports handled=true when the guard resolved the reload
// itself (refused it, or handed it to the coordinated rollout barrier), in which
// case the caller MUST return without swapping. handled=false means
// clusterReloadProceed: neither side is clustered, so the normal single-node
// apply path continues.
//
// newCfg is the caller's ORIGINAL config pointer, carried on SwapEvent purely
// for the config-manager's pointer-identity acknowledgement correlation;
// frozenCfg is the immutable content every decision is made against.
func (s *Supervisor) clusterReloadGuard(
	ctx context.Context,
	oldCfg, frozenCfg, newCfg *ports.BridgeConfig,
) bool {
	switch disp, reason := classifyClusterReload(oldCfg, frozenCfg); disp {
	case clusterReloadCoordinated:
		s.handleCoordinatedReload(ctx, oldCfg, frozenCfg, newCfg)
		return true
	case clusterReloadRefuse:
		s.refuseClusteredReload(oldCfg, frozenCfg, newCfg, reason)
		return true
	default:
		return false // clusterReloadProceed
	}
}

// handleCoordinatedReload routes a live-safe delta in a coordinated cluster into
// the rollout barrier, which drives the all-member commit. This node must not
// swap here under ANY outcome: swapping uncoordinated would split the cohort
// across config versions, which is the whole reason ADR 0012 refuses clustered
// live reloads. The local swap happens later, driven by the applier, once the
// barrier reaches Committed.
func (s *Supervisor) handleCoordinatedReload(
	ctx context.Context,
	oldCfg, frozenCfg, newCfg *ports.BridgeConfig,
) {
	if err := s.proposeCoordinatedRollout(ctx, oldCfg, frozenCfg, newCfg); err != nil {
		// Fail CLOSED: the running config keeps serving and the failure is
		// visible. Never emit a success-shaped Deferred event here — an in-band
		// applier resolves that as committed while nothing recorded the desired
		// config, silently DROPPING the change.
		if s.logger != nil {
			s.logger.Error("supervisor: coordinated cluster rollout could not be proposed; "+
				"refusing the live-safe delta fail-closed (the running config keeps serving)",
				"error", err, "attempted_config_version", frozenCfg.Version)
		}
		s.emitConfigReload(false)
		if s.onSwap != nil {
			s.safeOnSwap(SwapEvent{
				OldConfig: cloneConfigSnapshot(oldCfg),
				NewConfig: newCfg,
				Error:     err,
			})
		}
		return
	}
	// Proposed. The delta is committed-not-applied on this node until the
	// barrier reaches Committed, so the event is a DEFERRAL naming the barrier —
	// distinct from an admin pause, which needs operator action.
	if s.logger != nil {
		s.logger.Info("supervisor: live-safe delta proposed to the coordinated cluster rollout "+
			"barrier; this node applies it when the barrier commits",
			"attempted_config_version", frozenCfg.Version)
	}
	if s.onSwap != nil {
		s.safeOnSwap(SwapEvent{
			OldConfig:   cloneConfigSnapshot(oldCfg),
			NewConfig:   newCfg,
			Deferred:    true,
			DeferReason: DeferReasonRolloutPending,
		})
	}
}

// refuseClusteredReload fails a clustered live reload closed (ADR 0012). reason
// is non-empty only for a coordinated cluster whose delta is replacement-required
// — then it names the specific class reason on top of the generic refusal.
func (s *Supervisor) refuseClusteredReload(oldCfg, frozenCfg, newCfg *ports.BridgeConfig, reason string) {
	err := fmt.Errorf("bridge: refusing live reload of a clustered deployment: a per-process "+
		"reload has no cluster-wide version barrier or coordinated rollback, so a rolling reload "+
		"would split the cohort across config versions. Externally coordinate a whole-cohort "+
		"replacement (stage, validate every member, quiesce ingress, drain/stop all members, "+
		"commit, start all members, verify the version/readiness barrier, then re-enable ingress); "+
		"see docs/runbooks/cluster-config-rollout.md. WithAllowDestructiveReload does not apply "+
		"(attempted_config_version=%d)", frozenCfg.Version)
	if reason != "" {
		err = fmt.Errorf("%w; this delta is replacement-required and keeps the whole-cohort "+
			"replacement procedure: %s", err, reason)
	}
	if s.logger != nil {
		s.logger.Error("supervisor: refusing live reload of a clustered deployment; a per-process "+
			"reload has no cluster-wide version barrier — coordinate an external whole-cohort "+
			"stop/deploy/start (docs/runbooks/cluster-config-rollout.md)",
			"error", err, "attempted_config_version", frozenCfg.Version)
	}
	s.emitConfigReload(false)
	if s.onSwap != nil {
		s.safeOnSwap(SwapEvent{
			OldConfig: cloneConfigSnapshot(oldCfg),
			NewConfig: newCfg,
			Error:     err,
		})
	}
}

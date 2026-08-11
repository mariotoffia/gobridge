package gobridgedynamodbha

import (
	"fmt"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// Deployment safety switch for the autoscaled DynamoDB-coordinated HA facade
// Two rules, both enforced at synth time:
//
//   - a config that opts into the coordinated cluster rollout barrier is
//     rejected, because this profile's workers cannot host it; and
//   - every config change deploys by whole-cohort replacement, which the ECS
//     service properties in ha.go express and the advisory below states.
//
// The barrier itself is untouched. bridge.NewClusterRolloutDriver and the
// bootstrap rollout host stay available to custom composition roots and to a
// static member-slot profile that can supply restart-stable identities.

// wholeCohortAdvisory is the synth-time deployment event stating the profile's
// config-change capability and the price it pays for it. It is emitted as a CDK
// info annotation so it travels with the synthesized stack rather than living
// only in documentation.
//
// Be precise about what the service properties buy. MinimumHealthyPercent=0 /
// MaximumPercent=100 removes the lower bound and caps total tasks at the desired
// count, so no full second cohort ever exists — but it imposes no ORDER, and at
// a desired count of two or more the ECS scheduler may still replace in batches
// (stop one, start one). A store- or identity-incompatible revision therefore
// still needs the operator-run scale-to-zero procedure; the service property
// narrows the window, the runbook closes it.
const wholeCohortAdvisory = "GoBridgeDynamoDBHA: config changes deploy by whole-cohort replacement. " +
	"Both services deploy at MinimumHealthyPercent=0 / MaximumPercent=100 and the worker service is " +
	"ordered after the control service, so no second cohort runs beside the first and the config " +
	"seeder always precedes the workers it feeds. ECS may still replace workers in batches, so a " +
	"revision that changes durable session identity or store targets MUST use the scale-to-zero " +
	"procedure in docs/runbooks/cluster-config-rollout.md rather than a rolling update. This profile " +
	"has NO coordinated cluster rollout: its ECS worker tasks are interchangeable and carry no " +
	"restart-stable member_id. Costs to plan for: an ingress gap for the duration of every " +
	"replacement, no ECS Availability Zone rebalancing (AZ spread is best-effort at launch, not " +
	"continuously maintained), and the warm-standby and worker-degraded alarms breaching for the " +
	"length of each deploy. Batch config changes accordingly."

// clusterRolloutRemedy is the operator-facing action appended to every
// coordinated-rollout rejection. Keep it identical across the rejections so the
// text stays greppable in deployment logs.
const clusterRolloutRemedy = "Change config through whole-cohort replacement (stage, validate, quiesce " +
	"ingress, replace the cohort, verify the version/readiness barrier; see " +
	"docs/runbooks/cluster-config-rollout.md), or deploy a static member-slot profile in which every " +
	"member has a restart-stable member_id."

// rejectCoordinatedRollout refuses a coordinated-rollout cohort on this profile.
// The facade runs its workers as ONE autoscaled ECS service: every replacement
// task gets a fresh ECS task id, so no worker holds a restart-stable member_id.
// The barrier is built out of exactly that identity — the process announces its
// member_id, the roster in bridge.cluster.members IS the membership epoch the
// coordinator freezes, and acknowledgements are counted against it — so a cohort
// of interchangeable tasks can never reach a quorum, and a half-satisfied cohort
// would commit generations no member applies. Reject the shape at synth time
// rather than deploying a stack that can only fail at boot.
//
// An unset rollout and the explicit "refuse" are both accepted: "refuse" IS this
// profile's own policy (ADR 0012), spelled out. config.Validate has already
// rejected any other value by the time this runs.
//
// The coordinated test goes through bridge.IsCoordinatedRollout — the exported
// form of the barrier's OWN gate — rather than a local copy of the "coordinated"
// spelling, so this rejection cannot drift away from what actually enables the
// barrier.
func rejectCoordinatedRollout(cfg *ports.BridgeConfig) error {
	cluster := cfg.Bridge.Cluster
	if cluster == nil {
		return nil
	}
	if bridge.IsCoordinatedRollout(cfg) {
		return fmt.Errorf("bridge.cluster.rollout must be omitted or \"refuse\" (got %q): this profile "+
			"deploys interchangeable autoscaled ECS worker tasks, none of which carries the restart-stable "+
			"member_id the coordinated rollout barrier counts acknowledgements against. %s",
			cluster.Rollout, clusterRolloutRemedy)
	}
	if len(cluster.Members) > 0 {
		return fmt.Errorf("bridge.cluster.members must be omitted (got %d members): the roster names "+
			"coordinated-rollout cohort members by their restart-stable member_id, which interchangeable "+
			"autoscaled ECS worker tasks do not have, and it has no meaning without "+
			"bridge.cluster.rollout: coordinated. %s",
			len(cluster.Members), clusterRolloutRemedy)
	}
	return nil
}

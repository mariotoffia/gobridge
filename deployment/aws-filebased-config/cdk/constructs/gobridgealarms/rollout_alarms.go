package gobridgealarms

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

// Fleet convergence alarms for a coordinated cluster rollout cohort.
//
// The barrier is atomic BEFORE the commit and per-member AFTER it (ADR 0013), so
// the cohort's shared rollout row reads "committed" identically on a member that
// swapped and on one whose swap failed. No alarm derived from that row can tell
// them apart — these read the PER-MEMBER series instead, rolled up to the fleet.

const (
	// Coordinated cluster rollout convergence metrics (mirror
	// domain/shared.MetricClusterRollout*). Kept as literals because the CDK
	// constructs must not depend on the runtime domain module.
	metricClusterRolloutDiverged       = "ClusterRolloutDiverged"
	metricClusterRolloutTerminal       = "ClusterRolloutTerminal"
	metricClusterRolloutObservationAge = "ClusterRolloutObservationAge"

	// clusterRolloutObservationAgeThreshold is how many SECONDS a member's last
	// read of the rollout row may age before the fleet alarms. The barrier polls
	// every couple of seconds and calls its own status stale at three ticks, so a
	// minute is an order of magnitude outside any legitimate value — and every
	// rollout field an operator would act on is a projection of that read, so a
	// fleet that cannot see the row is a fleet whose rollout state is unknown,
	// not a healthy one.
	//
	// It assumes the default poll cadence, which is what every deployment this
	// bundle serves runs: the interval is not a bootstrap setting. A composition
	// root that slows the barrier below a 20-second poll would make each member's
	// own staleness budget outrun this alarm, and would need its own threshold.
	clusterRolloutObservationAgeThreshold = 60
)

// newClusterRolloutAlarms installs the fleet convergence alarms for a coordinated
// cohort. All three take the fleet MAXIMUM of a per-member gauge, so ONE member
// in the wrong state alarms — which is the point: a cohort is only converged if
// every member is.
func (g *GoBridgeAlarms) newClusterRolloutAlarms(scope constructs.Construct, ns string,
	period awscdk.Duration, evals *float64, action awscloudwatch.IAlarmAction,
) {
	// Diverged reads 1 while a member is not running the generation the cohort
	// decided on. A short 1 during a rollout is the normal per-member convergence
	// window, so the evaluation periods are what separate it from a split cohort.
	g.clusterRolloutDiverged = newRollupAlarm(scope, "HAClusterRolloutDiverged", ns,
		metricClusterRolloutDiverged, "Maximum", jsii.Number(0), period, evals, action,
		awscloudwatch.TreatMissingData_NOT_BREACHING,
		"A GoBridge cohort member is NOT running the cluster rollout generation the cohort decided on "+
			"(mixed generations). Read /deephealth config_watch.rollout on each member for which one.")
	// Terminal names a member that has exhausted its own repair. It is not a rate:
	// any non-zero value needs an operator, and deep health says which action.
	g.clusterRolloutTerminal = newRollupAlarm(scope, "HAClusterRolloutTerminal", ns,
		metricClusterRolloutTerminal, "Maximum", jsii.Number(0), period, evals, action,
		awscloudwatch.TreatMissingData_NOT_BREACHING,
		"A GoBridge cohort member cannot reach the safe state of a cluster rollout generation on its own. "+
			"Read terminal_reason in /deephealth: it says whether to repair the rollout store or replace the member.")
	// Observation age is the freshness of everything else. A stale fleet does not
	// know its own rollout state, which is not the same as being healthy.
	g.clusterRolloutObservationAge = newRollupAlarm(scope, "HAClusterRolloutObservationAge", ns,
		metricClusterRolloutObservationAge, "Maximum", jsii.Number(clusterRolloutObservationAgeThreshold),
		period, evals, action, awscloudwatch.TreatMissingData_NOT_BREACHING,
		"GoBridge cohort members have not read the cluster rollout row for over a minute; the rollout "+
			"state they report is out of date. Check the rollout table and the members' store credentials.")
}

// ClusterRolloutDivergedAlarm, ClusterRolloutTerminalAlarm and
// ClusterRolloutObservationAgeAlarm are the fleet convergence alarms; nil unless
// EnableClusterRolloutAlarms was set on an HA deployment.
func (g *GoBridgeAlarms) ClusterRolloutDivergedAlarm() awscloudwatch.IAlarm {
	return g.clusterRolloutDiverged
}

func (g *GoBridgeAlarms) ClusterRolloutTerminalAlarm() awscloudwatch.IAlarm {
	return g.clusterRolloutTerminal
}

func (g *GoBridgeAlarms) ClusterRolloutObservationAgeAlarm() awscloudwatch.IAlarm {
	return g.clusterRolloutObservationAge
}

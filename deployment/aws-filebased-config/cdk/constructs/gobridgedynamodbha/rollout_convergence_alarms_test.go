//go:build !race

package gobridgedynamodbha_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
)

// Fleet convergence alarms for a coordinated cluster rollout cohort.
//
// The barrier is atomic BEFORE the commit and per-member AFTER it (ADR 0013): the
// cohort decides once, then each member converges on its own. The shared rollout
// row therefore reads "committed" identically on a member that swapped and on one
// whose swap failed, so no alarm derived from that row can distinguish a
// converged cohort from a split one. These three read the per-member series,
// rolled up to the fleet maximum, so ONE wrong member alarms.

// TestGoBridgeDynamoDBHA_FleetConvergenceAlarmsAreOptIn proves the alarms are not
// installed by default. A deployment that does not run the barrier emits none of
// these series, and an alarm that can never leave INSUFFICIENT_DATA teaches
// operators to ignore the console.
func TestGoBridgeDynamoDBHA_FleetConvergenceAlarmsAreOptIn(t *testing.T) {
	h := newHAHarness(t, nil)
	topic := awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil)

	alarms := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge, Efs: h.bridge.EfsConfig(), AlarmTopic: topic,
	})

	if alarms.ClusterRolloutDivergedAlarm() != nil ||
		alarms.ClusterRolloutTerminalAlarm() != nil ||
		alarms.ClusterRolloutObservationAgeAlarm() != nil {
		t.Fatal("fleet convergence alarms must not be installed without EnableClusterRolloutAlarms")
	}
	for metric := range rolloutAlarmsByMetric(t, h) {
		t.Fatalf("rollout alarm %q present without EnableClusterRolloutAlarms", metric)
	}
}

// TestGoBridgeDynamoDBHA_FleetConvergenceAlarmsCoverEveryDivergenceSignal is the
// alarm half of the rollout convergence contract. A per-member apply failure is
// permitted by the protocol and invisible in every cohort-level signal, so the
// deployment has to alarm on it — otherwise "mixed generations are tolerated"
// silently becomes "mixed generations are unnoticed".
//
// The HA construct is used here for its metrics namespace, not because the
// alarms belong to it: the barrier runs wherever a composition root drives it,
// and the shape-independence is pinned in the alarms bundle's own suite.
func TestGoBridgeDynamoDBHA_FleetConvergenceAlarmsCoverEveryDivergenceSignal(t *testing.T) {
	h := newHAHarness(t, nil)
	topic := awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil)

	alarms := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge, Efs: h.bridge.EfsConfig(), AlarmTopic: topic,
		EnableClusterRolloutAlarms: true,
	})

	for name, alarm := range map[string]any{
		"ClusterRolloutDiverged":       alarms.ClusterRolloutDivergedAlarm(),
		"ClusterRolloutTerminal":       alarms.ClusterRolloutTerminalAlarm(),
		"ClusterRolloutObservationAge": alarms.ClusterRolloutObservationAgeAlarm(),
	} {
		if alarm == nil {
			t.Fatalf("accessor for %s returned nil when the fleet convergence alarms are enabled", name)
		}
	}

	byMetric := rolloutAlarmsByMetric(t, h)
	for _, tc := range []struct {
		metric    string
		threshold float64
	}{
		// Divergence and terminal are 0/1 gauges: anything above zero is a member
		// in the wrong state, so the threshold is zero and the statistic is the
		// fleet maximum.
		{metric: "ClusterRolloutDiverged", threshold: 0},
		{metric: "ClusterRolloutTerminal", threshold: 0},
		// The observation age is in seconds; a minute is far outside the barrier's
		// own poll cadence, so it can only mean the fleet stopped reading the row.
		{metric: "ClusterRolloutObservationAge", threshold: 60},
	} {
		props, ok := byMetric[tc.metric]
		if !ok {
			t.Fatalf("no fleet alarm on %q", tc.metric)
		}
		if got, _ := props["Threshold"].(float64); got != tc.threshold {
			t.Errorf("alarm %s threshold = %v, want %v", tc.metric, got, tc.threshold)
		}
		if got, _ := props["Statistic"].(string); got != "Maximum" {
			t.Errorf("alarm %s statistic = %q, want Maximum so one wrong member alarms the fleet", tc.metric, got)
		}
		if got, _ := props["ComparisonOperator"].(string); got != "GreaterThanThreshold" {
			t.Errorf("alarm %s comparison = %q, want GreaterThanThreshold", tc.metric, got)
		}
		// A dimensioned alarm never matches the zero-dimension rollup series the
		// exporter double-publishes, and a fleet alarm must not be per-instance.
		if dims, present := props["Dimensions"]; present {
			t.Errorf("alarm %s carries dimensions %v; a fleet rollup alarm must carry none", tc.metric, dims)
		}
		if got, _ := props["Namespace"].(string); got != h.bridge.MetricsNamespace() {
			t.Errorf("alarm %s namespace = %q, want the deployment's metrics namespace", tc.metric, got)
		}
	}
}

// rolloutAlarmsByMetric returns the synthesized alarm properties keyed by the
// cluster-rollout metric each one watches.
func rolloutAlarmsByMetric(t *testing.T, h *haHarness) map[string]map[string]any {
	t.Helper()
	want := map[string]bool{
		"ClusterRolloutDiverged":       true,
		"ClusterRolloutTerminal":       true,
		"ClusterRolloutObservationAge": true,
	}
	out := map[string]map[string]any{}
	resources := assertions.Template_FromStack(h.stack, nil).
		FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	if resources == nil {
		return out
	}
	for _, raw := range *resources {
		props, _ := (*raw)["Properties"].(map[string]any)
		metric, _ := props["MetricName"].(string)
		if want[metric] {
			out[metric] = props
		}
	}
	return out
}

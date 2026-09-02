//go:build !race

package gobridgealarms_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
)

// TestAlarms_ClusterRollout_InstalledForAnyDeploymentShape is the finding this
// test exists for: the fleet convergence alarms were installed only inside the
// DynamoDB-HA branch, and that facade REJECTS a coordinated cohort at synth. So
// the alarms could be created only where the barrier provably never runs, and
// were silently dropped for the shapes that can run it — three green alarms
// while a cohort split unnoticed.
func TestAlarms_ClusterRollout_InstalledForAnyDeploymentShape(t *testing.T) {
	h := newHarness(t)
	s := h.newSingle(t)

	g := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Single:                     s,
		Efs:                        s.EfsConfig(),
		AlarmTopic:                 h.topic,
		EnableClusterRolloutAlarms: true,
	})

	for name, alarm := range map[string]any{
		"ClusterRolloutDiverged":       g.ClusterRolloutDivergedAlarm(),
		"ClusterRolloutTerminal":       g.ClusterRolloutTerminalAlarm(),
		"ClusterRolloutObservationAge": g.ClusterRolloutObservationAgeAlarm(),
	} {
		if alarm == nil {
			t.Fatalf("%s alarm is nil on a deployment shape that can host the barrier", name)
		}
	}
	byMetric := clusterRolloutAlarmNamespaces(t, h.stack)
	for _, metric := range []string{"ClusterRolloutDiverged", "ClusterRolloutTerminal", "ClusterRolloutObservationAge"} {
		ns, ok := byMetric[metric]
		if !ok {
			t.Fatalf("no alarm emitted on %q", metric)
		}
		if ns != "GoBridge/Runtime" {
			t.Fatalf("alarm %s namespace = %q, want the runtime rollup namespace", metric, ns)
		}
	}
}

// TestAlarms_ClusterRollout_HonoursTheRollupNamespaceOverride keeps the alarms
// pointed at the namespace the exporter actually publishes to. A mismatch is
// invisible: the alarm simply never leaves INSUFFICIENT_DATA.
func TestAlarms_ClusterRollout_HonoursTheRollupNamespaceOverride(t *testing.T) {
	h := newHarness(t)
	s := h.newSingle(t)

	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Single:                     s,
		Efs:                        s.EfsConfig(),
		AlarmTopic:                 h.topic,
		EnableClusterRolloutAlarms: true,
		RollupMetricsNamespace:     jsii.String("Acme/Bridge"),
	})

	if ns := clusterRolloutAlarmNamespaces(t, h.stack)["ClusterRolloutDiverged"]; ns != "Acme/Bridge" {
		t.Fatalf("ClusterRolloutDiverged namespace = %q, want the override", ns)
	}
}

// clusterRolloutAlarmNamespaces maps each emitted cluster-rollout alarm to the
// CloudWatch namespace it reads.
func clusterRolloutAlarmNamespaces(t *testing.T, stack awscdk.Stack) map[string]string {
	t.Helper()
	want := map[string]bool{
		"ClusterRolloutDiverged":       true,
		"ClusterRolloutTerminal":       true,
		"ClusterRolloutObservationAge": true,
	}
	out := map[string]string{}
	alarms := assertions.Template_FromStack(stack, nil).FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	if alarms == nil {
		return out
	}
	for _, raw := range *alarms {
		props := (*raw)["Properties"].(map[string]any)
		metric, _ := props["MetricName"].(string)
		if want[metric] {
			ns, _ := props["Namespace"].(string)
			out[metric] = ns
		}
	}
	return out
}

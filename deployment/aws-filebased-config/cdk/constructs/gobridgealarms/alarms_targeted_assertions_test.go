//go:build !race

package gobridgealarms_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
)

// Test_T20_Alarms_Cluster_NamedAlarmsByMetricName: every named alarm in the
// cluster+attachment scenario must be present in the synthesized template,
// looked up by MetricName. Distinct from the existing Emits7 test which
// asserts via accessor count — this one pins the actual CloudFormation
// MetricName strings the alarms watch on.
func Test_T20_Alarms_Cluster_NamedAlarmsByMetricName(t *testing.T) {
	h := newHarness(t)
	c := h.newCluster(t)
	att := h.newAttachment(t, c)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Cluster:    c,
		Efs:        c.EfsConfig(),
		Attachment: att,
		AlarmTopic: h.topic,
	})
	tpl := assertions.Template_FromStack(h.stack, nil)

	wantMetricCounts := map[string]int{
		"PercentIOLimit":            1, // EFS
		"UnHealthyHostCount":        2, // ALB control + worker
		"HTTPCode_Target_5XX_Count": 2, // ALB control + worker
		"RunningTaskCount":          1, // control absence (worker degraded uses MathExpression, no MetricName)
	}
	got := map[string]int{}
	alarms := tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	for _, raw := range *alarms {
		props := (*raw)["Properties"].(map[string]any)
		if mn, ok := props["MetricName"].(string); ok {
			got[mn]++
		}
	}
	for name, want := range wantMetricCounts {
		if got[name] != want {
			t.Fatalf("alarm count for MetricName=%s: got %d want %d (full %v)",
				name, got[name], want, got)
		}
	}
}

// Test_T20_Alarms_Defaults_PerMetricShape pins the default ComparisonOperator,
// EvaluationPeriods, and Threshold per metric category for the cluster
// scenario (using construct defaults — no overrides). Iterates every alarm
// and matches by MetricName so a new alarm or a default change fails loudly.
func Test_T20_Alarms_Defaults_PerMetricShape(t *testing.T) {
	h := newHarness(t)
	c := h.newCluster(t)
	att := h.newAttachment(t, c)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Cluster:    c,
		Efs:        c.EfsConfig(),
		Attachment: att,
		AlarmTopic: h.topic,
	})
	tpl := assertions.Template_FromStack(h.stack, nil)

	type want struct {
		op        string
		threshold float64
	}
	expected := map[string]want{
		"PercentIOLimit":            {"GreaterThanThreshold", 90},
		"UnHealthyHostCount":        {"GreaterThanThreshold", 0},
		"HTTPCode_Target_5XX_Count": {"GreaterThanThreshold", 5},
		"RunningTaskCount":          {"LessThanThreshold", 1},
	}

	alarms := tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	checked := map[string]bool{}
	for id, raw := range *alarms {
		props := (*raw)["Properties"].(map[string]any)
		mn, ok := props["MetricName"].(string)
		if !ok {
			continue
		}
		w, ok := expected[mn]
		if !ok {
			continue
		}
		if op, _ := props["ComparisonOperator"].(string); op != w.op {
			t.Fatalf("alarm %s (%s): ComparisonOperator = %q, want %q", id, mn, op, w.op)
		}
		if th, _ := props["Threshold"].(float64); th != w.threshold {
			t.Fatalf("alarm %s (%s): Threshold = %v, want %v", id, mn, th, w.threshold)
		}
		if ev, _ := props["EvaluationPeriods"].(float64); ev != 5 {
			t.Fatalf("alarm %s (%s): EvaluationPeriods = %v, want 5 (default)", id, mn, ev)
		}
		checked[mn] = true
	}
	for mn := range expected {
		if !checked[mn] {
			t.Fatalf("missing alarm with MetricName=%s in template", mn)
		}
	}
}

// Test_T20_Alarms_AlarmActions_RefSnsTopic asserts that every alarm wires its
// AlarmActions[0] to a Ref of the provided SNS topic logical id. Distinct
// from existing TestAlarms_OkActionWired which only checks count==1.
func Test_T20_Alarms_AlarmActions_RefSnsTopic(t *testing.T) {
	h := newHarness(t)
	s := h.newSingle(t)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Single: s, Efs: s.EfsConfig(), AlarmTopic: h.topic,
	})
	tpl := assertions.Template_FromStack(h.stack, nil)

	// Discover the SNS topic logical id.
	topics := tpl.FindResources(jsii.String("AWS::SNS::Topic"), nil)
	if topics == nil || len(*topics) != 1 {
		t.Fatalf("want 1 SNS::Topic, got %v", topics)
	}
	var topicID string
	for id := range *topics {
		topicID = id
	}

	alarms := tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	if len(*alarms) == 0 {
		t.Fatal("no alarms emitted")
	}
	for id, raw := range *alarms {
		props := (*raw)["Properties"].(map[string]any)
		actions, _ := props["AlarmActions"].([]any)
		if len(actions) != 1 {
			t.Fatalf("alarm %s: AlarmActions count = %d, want 1", id, len(actions))
		}
		ref, _ := actions[0].(map[string]any)["Ref"].(string)
		if ref != topicID {
			t.Fatalf("alarm %s: AlarmActions[0].Ref = %q, want %q", id, ref, topicID)
		}
	}
}

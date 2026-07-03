//go:build !race

package gobridgealarms_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
)

// TestAlarms_Rollup_OptIn asserts the custom rollup-metric alarms are only
// materialized when EnableRollupAlarms is set, target the correct namespace,
// and expose accessors.
func TestAlarms_Rollup_OptIn(t *testing.T) {
	defer jsii.Close()

	t.Run("off-by-default", func(t *testing.T) {
		h := newHarness(t)
		s := h.newSingle(t)
		gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
			Single:     s,
			Efs:        s.EfsConfig(),
			AlarmTopic: h.topic,
		})
		names := rollupMetricNames(t, h.stack)
		for _, m := range []string{"OutboxDepth", "DLQEntries", "LeaseExpiries", "LeaseAcquireFailures"} {
			if names[m] {
				t.Fatalf("rollup metric %q alarm present without EnableRollupAlarms", m)
			}
		}
	})

	t.Run("enabled-emits-four-in-runtime-namespace", func(t *testing.T) {
		h := newHarness(t)
		s := h.newSingle(t)
		g := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
			Single:             s,
			Efs:                s.EfsConfig(),
			AlarmTopic:         h.topic,
			EnableRollupAlarms: true,
		})
		for _, a := range []struct {
			name  string
			alarm any
		}{
			{"OutboxDepth", g.OutboxDepthAlarm()},
			{"DLQEntries", g.DLQEntriesAlarm()},
			{"LeaseExpiries", g.LeaseExpiriesAlarm()},
			{"LeaseAcquireFailures", g.LeaseAcquireFailuresAlarm()},
		} {
			if a.alarm == nil {
				t.Fatalf("accessor for %s returned nil when rollup alarms enabled", a.name)
			}
		}

		byNamespace := rollupMetricNamespaces(t, h.stack)
		for _, m := range []string{"OutboxDepth", "DLQEntries", "LeaseExpiries", "LeaseAcquireFailures"} {
			ns, ok := byNamespace[m]
			if !ok {
				t.Fatalf("rollup metric %q alarm not emitted", m)
			}
			if ns != "GoBridge/Runtime" {
				t.Fatalf("rollup metric %q namespace = %q, want GoBridge/Runtime", m, ns)
			}
		}
	})

	t.Run("custom-namespace-and-thresholds", func(t *testing.T) {
		h := newHarness(t)
		s := h.newSingle(t)
		gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
			Single:                 s,
			Efs:                    s.EfsConfig(),
			AlarmTopic:             h.topic,
			EnableRollupAlarms:     true,
			RollupMetricsNamespace: jsii.String("Acme/Bridge"),
			OutboxDepthThreshold:   jsii.Number(5000),
		})
		byNamespace := rollupMetricNamespaces(t, h.stack)
		if byNamespace["OutboxDepth"] != "Acme/Bridge" {
			t.Fatalf("OutboxDepth namespace override not applied: %q", byNamespace["OutboxDepth"])
		}
	})
}

// rollupMetricNames returns the set of custom rollup metric names that have an
// alarm emitted in the template.
func rollupMetricNames(t *testing.T, stack awscdk.Stack) map[string]bool {
	out := map[string]bool{}
	for m := range rollupMetricNamespaces(t, stack) {
		out[m] = true
	}
	return out
}

// rollupMetricNamespaces maps each emitted rollup-metric alarm to its
// CloudWatch namespace.
func rollupMetricNamespaces(t *testing.T, stack awscdk.Stack) map[string]string {
	t.Helper()
	tpl := assertions.Template_FromStack(stack, nil)
	alarms := tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	out := map[string]string{}
	if alarms == nil {
		return out
	}
	want := map[string]bool{"OutboxDepth": true, "DLQEntries": true, "LeaseExpiries": true, "LeaseAcquireFailures": true}
	for _, raw := range *alarms {
		props := (*raw)["Properties"].(map[string]any)
		metricName, _ := props["MetricName"].(string)
		if !want[metricName] {
			continue
		}
		ns, _ := props["Namespace"].(string)
		out[metricName] = ns
	}
	return out
}

package cloudwatch

import (
	"testing"

	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Verifies DefaultAlarms returns the expected number of predefined alarms.
func TestDefaultAlarms_Count(t *testing.T) {
	alarms := DefaultAlarms("", "")
	if len(alarms) != 9 {
		t.Fatalf("expected 9 default alarms, got %d", len(alarms))
	}
}

// Verifies alarm namespace defaults to the bridge metric namespace or a custom override.
func TestDefaultAlarms_Namespace(t *testing.T) {
	alarms := DefaultAlarms("", "")
	for _, a := range alarms {
		if a.Namespace != shared.MetricNamespace {
			t.Errorf("alarm %s namespace = %q, want %q", a.Name, a.Namespace, shared.MetricNamespace)
		}
	}

	custom := DefaultAlarms("Custom/NS", "")
	for _, a := range custom {
		if a.Namespace != "Custom/NS" {
			t.Errorf("alarm %s namespace = %q, want Custom/NS", a.Name, a.Namespace)
		}
	}
}

// Verifies each alarm carries the configured SNS topic ARN for notifications.
func TestDefaultAlarms_SNSTopic(t *testing.T) {
	arn := "arn:aws:sns:eu-west-1:123456:alarms"
	alarms := DefaultAlarms("", arn)
	for _, a := range alarms {
		if a.SNSTopicARN != arn {
			t.Errorf("alarm %s SNSTopicARN = %q, want %q", a.Name, a.SNSTopicARN, arn)
		}
	}
}

// Verifies default alarms cover each required bridge metric name.
func TestDefaultAlarms_MetricNames(t *testing.T) {
	alarms := DefaultAlarms("", "")
	want := map[string]bool{
		shared.MetricOutboxDepth:          false,
		shared.MetricLeaseExpiries:        false,
		shared.MetricDLQEntries:           false,
		shared.MetricLeaseAcquireFailures: false,
		"SQSVisibilityExtensions":         false,
		shared.MetricDLQDepth:             false,
		shared.MetricMessagesDropped:      false,
		shared.MetricMessagesExpired:      false,
	}
	for _, a := range alarms {
		want[a.MetricName] = true
	}
	for metric, found := range want {
		if !found {
			t.Errorf("expected alarm for metric %s", metric)
		}
	}
}

// Verifies default alarms use the expected warning versus critical severity split.
func TestDefaultAlarms_Severities(t *testing.T) {
	alarms := DefaultAlarms("", "")
	warnings := 0
	criticals := 0
	for _, a := range alarms {
		switch a.Severity {
		case SeverityWarning:
			warnings++
		case SeverityCritical:
			criticals++
		}
	}
	if warnings != 6 {
		t.Errorf("expected 6 WARNING alarms, got %d", warnings)
	}
	if criticals != 3 {
		t.Errorf("expected 3 CRITICAL alarms, got %d", criticals)
	}
}

// OutboxDepth is a continuously emitted gauge — missing data means
// the emitter is dead, so its alarms treat missing data as breaching.
// Event counters treat missing data as notBreaching (no events = healthy).
func TestDefaultAlarms_TreatMissingData(t *testing.T) {
	for _, a := range DefaultAlarms("", "") {
		want := "notBreaching"
		if a.MetricName == shared.MetricOutboxDepth {
			want = "breaching"
		}
		if a.TreatMissingData != want {
			t.Errorf("alarm %s TreatMissingData = %q, want %q", a.Name, a.TreatMissingData, want)
		}
	}
}

// H-OBS: the silent-loss counters and the DLQ-depth backlog gauge must be in
// the default rollup set, else a dimensionless fleet alarm can never match
// their route/partition-dimensioned base series. Fails before the fix
// that added them to DefaultRollupMetrics.
func TestDefaultRollupMetrics_CoversSilentLossCounters(t *testing.T) {
	rollups := map[string]bool{}
	for _, name := range DefaultRollupMetrics() {
		rollups[name] = true
	}
	for _, want := range []string{
		shared.MetricDLQDepth,
		shared.MetricMessagesDropped,
		shared.MetricMessagesExpired,
		shared.MetricMessagesFiltered,
	} {
		if !rollups[want] {
			t.Errorf("DefaultRollupMetrics() missing silent-loss metric %q", want)
		}
	}
}

// H-OBS: message loss must be alarmable out-of-the-box. A terminal drop is
// critical (any drop), sustained expiry is a warning, and a non-empty DLQ is a
// warning. Fails before the fix that shipped these default alarms.
func TestDefaultAlarms_ShipSilentLossAlarms(t *testing.T) {
	byMetric := map[string]AlarmDefinition{}
	for _, a := range DefaultAlarms("", "") {
		byMetric[a.MetricName] = a
	}

	drop, ok := byMetric[shared.MetricMessagesDropped]
	if !ok {
		t.Fatalf("no default alarm for %s (silent message loss)", shared.MetricMessagesDropped)
	}
	if drop.Severity != SeverityCritical {
		t.Errorf("MessagesDropped alarm severity = %q, want CRITICAL", drop.Severity)
	}
	if drop.Threshold != 0 || drop.Comparison != cwtypes.ComparisonOperatorGreaterThanThreshold {
		t.Errorf("MessagesDropped alarm should fire on > 0, got threshold=%v comparison=%v", drop.Threshold, drop.Comparison)
	}

	exp, ok := byMetric[shared.MetricMessagesExpired]
	if !ok {
		t.Fatalf("no default alarm for %s (TTL loss)", shared.MetricMessagesExpired)
	}
	if exp.EvalPeriods < 2 {
		t.Errorf("MessagesExpired alarm should be SUSTAINED (EvalPeriods >= 2), got %d", exp.EvalPeriods)
	}

	dlq, ok := byMetric[shared.MetricDLQDepth]
	if !ok {
		t.Fatalf("no default alarm for %s (DLQ backlog)", shared.MetricDLQDepth)
	}
	if dlq.Statistic != cwtypes.StatisticMaximum {
		t.Errorf("DLQDepth alarm statistic = %q, want Maximum (it is a gauge)", dlq.Statistic)
	}
}

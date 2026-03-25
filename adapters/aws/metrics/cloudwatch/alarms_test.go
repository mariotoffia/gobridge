package cloudwatch

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

// Verifies DefaultAlarms returns the expected number of predefined alarms.
func TestDefaultAlarms_Count(t *testing.T) {
	alarms := DefaultAlarms("", "")
	if len(alarms) != 6 {
		t.Fatalf("expected 6 default alarms, got %d", len(alarms))
	}
}

// Verifies alarm namespace defaults to the bridge metric namespace or a custom override.
func TestDefaultAlarms_Namespace(t *testing.T) {
	alarms := DefaultAlarms("", "")
	for _, a := range alarms {
		if a.Namespace != domain.MetricNamespace {
			t.Errorf("alarm %s namespace = %q, want %q", a.Name, a.Namespace, domain.MetricNamespace)
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
		domain.MetricOutboxDepth:             false,
		domain.MetricLeaseExpiries:           false,
		domain.MetricDLQEntries:              false,
		domain.MetricLeaseAcquireFailures:    false,
		domain.MetricSQSVisibilityExtensions: false,
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
	if warnings != 4 {
		t.Errorf("expected 4 WARNING alarms, got %d", warnings)
	}
	if criticals != 2 {
		t.Errorf("expected 2 CRITICAL alarms, got %d", criticals)
	}
}

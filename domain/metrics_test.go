package domain_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

// TestMetricNamespace_NonEmpty validates that MetricNamespace is a non-empty string.
func TestMetricNamespace_NonEmpty(t *testing.T) {
	if domain.MetricNamespace == "" {
		t.Fatal("MetricNamespace must not be empty")
	}
}

// TestMetricConstants_NonEmpty validates that all Metric* name constants are non-empty
// and unique across the entire set.
func TestMetricConstants_NonEmpty(t *testing.T) {
	metrics := []string{
		domain.MetricLeaseAcquireLatency,
		domain.MetricLeaseRenewLatency,
		domain.MetricLeaseAcquireFailures,
		domain.MetricLeaseExpiries,
		domain.MetricLeaseTransfers,
		domain.MetricOutboxPersistLatency,
		domain.MetricOutboxDrainLatency,
		domain.MetricOutboxDepth,
		domain.MetricOutboxClaimRecoveries,
		domain.MetricOutboxCompletions,
		domain.MetricOutboxExpiredBeforeSend,
		domain.MetricOutboxReplayCount,
		domain.MetricSQSReceiveLatency,
		domain.MetricSQSDeleteLatency,
		domain.MetricSQSVisibilityExtensions,
		domain.MetricDeliveryE2ELatency,
		domain.MetricDLQEntries,
		domain.MetricMQTTPublishLatency,
		domain.MetricMQTTReconnects,
	}

	seen := make(map[string]bool, len(metrics))
	for _, m := range metrics {
		if m == "" {
			t.Fatal("metric constant must not be empty")
		}
		if seen[m] {
			t.Fatalf("duplicate metric constant: %q", m)
		}
		seen[m] = true
	}
}

// TestTagKeyConstants_NonEmpty validates that all TagKey* constants are non-empty and unique.
func TestTagKeyConstants_NonEmpty(t *testing.T) {
	tagKeys := []string{
		domain.TagKeyLeaseID,
		domain.TagKeyRouteID,
		domain.TagKeySessionID,
		domain.TagKeyPartition,
		domain.TagKeyQueueURL,
		domain.TagKeyCategory,
	}

	seen := make(map[string]bool, len(tagKeys))
	for _, k := range tagKeys {
		if k == "" {
			t.Fatal("tag key constant must not be empty")
		}
		if seen[k] {
			t.Fatalf("duplicate tag key constant: %q", k)
		}
		seen[k] = true
	}
}

// TestTag_Construction validates Tag struct creation with Key and Value fields.
func TestTag_Construction(t *testing.T) {
	tag := domain.Tag{Key: "route_id", Value: "r1"}
	if tag.Key != "route_id" {
		t.Fatalf("Tag.Key: got %q, want %q", tag.Key, "route_id")
	}
	if tag.Value != "r1" {
		t.Fatalf("Tag.Value: got %q, want %q", tag.Value, "r1")
	}
}

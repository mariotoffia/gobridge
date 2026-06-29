package shared_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestMetricNamespace_NonEmpty validates that MetricNamespace is a non-empty string.
func TestMetricNamespace_NonEmpty(t *testing.T) {
	if shared.MetricNamespace == "" {
		t.Fatal("MetricNamespace must not be empty")
	}
}

// TestMetricConstants_NonEmpty validates that all Metric* name constants are non-empty
// and unique across the entire set.
func TestMetricConstants_NonEmpty(t *testing.T) {
	metrics := []string{
		shared.MetricLeaseAcquireLatency,
		shared.MetricLeaseRenewLatency,
		shared.MetricLeaseAcquireFailures,
		shared.MetricLeaseExpiries,
		shared.MetricLeaseTransfers,
		shared.MetricOutboxPersistLatency,
		shared.MetricOutboxDrainLatency,
		shared.MetricOutboxDepth,
		shared.MetricOutboxClaimRecoveries,
		shared.MetricOutboxCompletions,
		shared.MetricOutboxExpiredBeforeSend,
		shared.MetricOutboxReplayCount,
		shared.MetricOutboxRecordFailures,
		shared.MetricAckLatency,
		shared.MetricVisibilityExtensions,
		shared.MetricDeliveryE2ELatency,
		shared.MetricDLQEntries,
		shared.MetricDeliveryPanics,
		shared.MetricMQTTReconnects,
		shared.MetricReconcileFailures,
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
		shared.TagKeyLeaseID,
		shared.TagKeyRouteID,
		shared.TagKeySessionID,
		shared.TagKeyPartition,
		shared.TagKeyCategory,
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
	tag := shared.Tag{Key: "route_id", Value: "r1"}
	if tag.Key != "route_id" {
		t.Fatalf("Tag.Key: got %q, want %q", tag.Key, "route_id")
	}
	if tag.Value != "r1" {
		t.Fatalf("Tag.Value: got %q, want %q", tag.Value, "r1")
	}
}

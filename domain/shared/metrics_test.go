package shared_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestMetricConstants_TransportAgnostic enforces Finding 12's invariant: the
// shared kernel (domain/shared/metrics.go) must contain ONLY generic,
// transport-agnostic metric names. A contributor must not be able to add a
// provider-flavored metric (e.g. MetricSQSPolls = "SQSPolls") to the shared
// kernel and keep lint+test green.
//
// MetricMQTTReconnects is the sole allow-listed exception: it carries the
// historical "MQTTReconnects" wire value for observability compatibility, and
// is emitted by the generic runtime session manager (runtime/session), not by
// the MQTT adapter. See metrics.go for the wire-compat rationale.
func TestMetricConstants_TransportAgnostic(t *testing.T) {
	tokens := []string{
		"SQS", "Azure", "ServiceBus", "AMQP", "Kafka", "Kinesis",
		"Dynamo", "SNS", "Paho", "RabbitMQ", "MQTT",
	}

	// allowed values exempt from the token scan (historical wire-compat).
	allowed := map[string]bool{
		shared.MetricMQTTReconnects: true,
	}

	values := []string{
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
		shared.MetricOutboxDuplicateRisk,
		shared.MetricAckLatency,
		shared.MetricVisibilityExtensions,
		shared.MetricDeliveryE2ELatency,
		shared.MetricDLQEntries,
		shared.MetricDLQWriteFailures,
		shared.MetricDeliveryPanics,
		shared.MetricMessagesReceived,
		shared.MetricMessagesSent,
		shared.MetricMessagesDropped,
		shared.MetricRouteErrors,
		shared.MetricReceiveCountUnparseable,
		shared.MetricProcessorPanics,
		shared.MetricProcessorTimeouts,
		shared.MetricMQTTReconnects,
		shared.MetricReconcileFailures,
		shared.MetricSessionRestarts,
	}

	for _, v := range values {
		if allowed[v] {
			continue
		}
		lower := strings.ToLower(v)
		for _, tok := range tokens {
			if strings.Contains(lower, strings.ToLower(tok)) {
				t.Errorf("shared metric %q contains transport/provider token %q; "+
					"the shared kernel must hold only generic, transport-agnostic metrics", v, tok)
			}
		}
	}
}

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
		shared.MetricOutboxDuplicateRisk,
		shared.MetricAckLatency,
		shared.MetricVisibilityExtensions,
		shared.MetricDeliveryE2ELatency,
		shared.MetricDLQEntries,
		shared.MetricDLQWriteFailures,
		shared.MetricDeliveryPanics,
		shared.MetricMessagesReceived,
		shared.MetricMessagesSent,
		shared.MetricMessagesDropped,
		shared.MetricRouteErrors,
		shared.MetricReceiveCountUnparseable,
		shared.MetricProcessorPanics,
		shared.MetricProcessorTimeouts,
		shared.MetricMQTTReconnects,
		shared.MetricReconcileFailures,
		shared.MetricSessionRestarts,
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
		shared.TagKeyTransport,
		shared.TagKeyEntity,
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

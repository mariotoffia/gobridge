package domain

// Tag is a key-value pair used as a dimension on emitted metrics.
type Tag struct {
	Key   string
	Value string
}

// Metric namespace used by the bridge runtime.
const MetricNamespace = "GoBridge/Runtime"

// Lease metric names.
const (
	MetricLeaseAcquireLatency  = "LeaseAcquireLatency"
	MetricLeaseRenewLatency    = "LeaseRenewLatency"
	MetricLeaseAcquireFailures = "LeaseAcquireFailures"
	MetricLeaseExpiries        = "LeaseExpiries"
	MetricLeaseTransfers       = "LeaseTransfers"
)

// Outbox metric names.
const (
	MetricOutboxPersistLatency    = "OutboxPersistLatency"
	MetricOutboxDrainLatency      = "OutboxDrainLatency"
	MetricOutboxDepth             = "OutboxDepth"
	MetricOutboxClaimRecoveries   = "OutboxClaimRecoveries"
	MetricOutboxCompletions       = "OutboxCompletions"
	MetricOutboxExpiredBeforeSend = "OutboxExpiredBeforeSend"
	MetricOutboxReplayCount       = "OutboxReplayCount"
)

// SQS metric names.
const (
	MetricSQSReceiveLatency       = "SQSReceiveLatency"
	MetricSQSDeleteLatency        = "SQSDeleteLatency"
	MetricSQSVisibilityExtensions = "SQSVisibilityExtensions"
)

// Delivery metric names.
const (
	MetricDeliveryE2ELatency = "DeliveryE2ELatency"
	MetricDLQEntries         = "DLQEntries"
)

// MQTT metric names.
const (
	MetricMQTTPublishLatency = "MQTTPublishLatency"
	MetricMQTTReconnects     = "MQTTReconnects"
)

// Standard dimension key names for metric tags.
const (
	TagKeyLeaseID   = "lease_id"
	TagKeyRouteID   = "route_id"
	TagKeySessionID = "session_id"
	TagKeyPartition = "partition"
	TagKeyQueueURL  = "queue_url"
	TagKeyCategory  = "category"
)

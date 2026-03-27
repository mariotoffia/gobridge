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
	MetricOutboxRecordFailures    = "OutboxRecordFailures"
)

// SQS metric names.
const (
	MetricSQSReceiveLatency       = "SQSReceiveLatency"
	MetricSQSDeleteLatency        = "SQSDeleteLatency"
	MetricSQSVisibilityExtensions = "SQSVisibilityExtensions"
)

// Generic transport-agnostic delivery metric names.
const (
	MetricAckLatency           = "AckLatency"
	MetricVisibilityExtensions = "VisibilityExtensions"
)

// Delivery metric names.
const (
	MetricDeliveryE2ELatency = "DeliveryE2ELatency"
	MetricDLQEntries         = "DLQEntries"
	MetricDeliveryPanics     = "DeliveryPanics"
)

// MQTT metric names.
const (
	MetricMQTTPublishLatency = "MQTTPublishLatency"
	MetricMQTTReconnects     = "MQTTReconnects"
)

// Session metric names.
const (
	MetricReconcileFailures = "ReconcileFailures"
)

// HTTP transport metric names.
const (
	MetricHTTPIngressLatency  = "HTTPIngressLatency"
	MetricHTTPForwardLatency  = "HTTPForwardLatency"
	MetricSSEClients          = "SSEClients"
	MetricSSEBroadcastLatency = "SSEBroadcastLatency"
	MetricClusterForwards     = "ClusterForwards"
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

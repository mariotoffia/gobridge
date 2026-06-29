package shared

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
	MetricOutboxDuplicateRisk     = "OutboxDuplicateRisk"
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
	MetricDLQBufferOverflow  = "DLQBufferOverflow"
	MetricDLQWriteFailures   = "DLQWriteFailures"
	MetricDeliveryPanics     = "DeliveryPanics"
)

// Throughput metric names.
const (
	MetricMessagesReceived = "MessagesReceived"
	MetricMessagesSent     = "MessagesSent"
	MetricMessagesDropped  = "MessagesDropped"
	MetricRouteErrors      = "RouteErrors"
)

// Processor chain metric names.
const (
	MetricProcessorPanics   = "ProcessorPanics"
	MetricProcessorTimeouts = "ProcessorTimeouts"
)

// Session metric names. Emitted by the generic runtime session manager
// (runtime/session), not by any single transport adapter.
// MetricMQTTReconnects keeps its historical "MQTTReconnects" wire value to
// avoid an observability break, despite the transport-flavored identifier.
const (
	MetricMQTTReconnects    = "MQTTReconnects"
	MetricReconcileFailures = "ReconcileFailures"
)

// Standard dimension key names for metric tags.
const (
	TagKeyLeaseID   = "lease_id"
	TagKeyRouteID   = "route_id"
	TagKeySessionID = "session_id"
	TagKeyPartition = "partition"
	TagKeyCategory  = "category"
	TagKeyTransport = "transport"
	TagKeyEntity    = "entity"
)

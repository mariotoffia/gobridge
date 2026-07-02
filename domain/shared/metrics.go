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
	MetricDLQWriteFailures   = "DLQWriteFailures"
	MetricDeliveryPanics     = "DeliveryPanics"
)

// Throughput metric names.
const (
	MetricMessagesReceived = "MessagesReceived"
	MetricMessagesSent     = "MessagesSent"
	MetricMessagesDropped  = "MessagesDropped"
	MetricRouteErrors      = "RouteErrors"
	// MetricReceiveCountUnparseable counts deliveries whose source-transport
	// redelivery-count header was PRESENT but uninterpretable as an integer, so
	// receiveCount failed open to a first delivery (count 0) and
	// MaxReplayAttempts could not cap replays (E5-FU3). Failing open is
	// deliberate — a good message is never DLQ'd on a parse error — but a
	// permanently-failing recoverable send on such a message would otherwise
	// retry unbounded with no signal; a rising value makes that observable and
	// flags a source stamping a malformed count.
	MetricReceiveCountUnparseable = "ReceiveCountUnparseable"
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
	// MetricSessionRestarts counts per-session supervised restarts: a session
	// manager returned a transient error and was restarted in isolation
	// (capped backoff) instead of tearing down the whole runtime (C3-FU2).
	// A rising value flags a session that keeps failing to reconnect/re-acquire
	// its lease while the rest of the bridge stays up — alert on it.
	MetricSessionRestarts = "SessionRestarts"
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

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
	MetricSQSPollLatency          = "SQSPollLatency"
	MetricSQSSendLatency          = "SQSSendLatency"
	MetricSQSSendBatchLatency     = "SQSSendBatchLatency"
	MetricSQSAutoExtends          = "SQSAutoExtends"
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

// MQTT metric names.
const (
	MetricMQTTPublishLatency  = "MQTTPublishLatency"
	MetricMQTTPublishFailures = "MQTTPublishFailures"
	MetricMQTTHandlerPanics   = "MQTTHandlerPanics"
	MetricMQTTReconnects      = "MQTTReconnects"
	MetricMQTTConnectLatency  = "MQTTConnectLatency"
	MetricMQTTReconcileLatency = "MQTTReconcileLatency"
	MetricMQTTRouterDropped   = "MQTTRouterDropped"
	MetricMQTTEventDropped    = "MQTTEventDropped"
)

// Session metric names.
const (
	MetricReconcileFailures = "ReconcileFailures"
)

// Azure Service Bus metric names.
const (
	MetricASBReceiveLatency   = "ASBReceiveLatency"
	MetricASBCompleteLatency  = "ASBCompleteLatency"
	MetricASBSendLatency      = "ASBSendLatency"
	MetricASBSendBatchLatency = "ASBSendBatchLatency"
	MetricASBScheduleLatency  = "ASBScheduleLatency"
	MetricASBLockRenewals     = "ASBLockRenewals"
)

// HTTP transport metric names.
const (
	MetricHTTPIngressLatency  = "HTTPIngressLatency"
	MetricHTTPForwardLatency  = "HTTPForwardLatency"
	MetricSSEClients          = "SSEClients"
	MetricSSEBroadcastLatency = "SSEBroadcastLatency"
	MetricClusterForwards     = "ClusterForwards"
)

// AMQP 0-9-1 metric names.
const (
	MetricAMQP091ConnectLatency   = "AMQP091ConnectLatency"
	MetricAMQP091ReconcileLatency = "AMQP091ReconcileLatency"
	MetricAMQP091PublishLatency   = "AMQP091PublishLatency"
	MetricAMQP091ConsumeLatency   = "AMQP091ConsumeLatency"
	MetricAMQP091AckLatency       = "AMQP091AckLatency"
	MetricAMQP091Reconnects       = "AMQP091Reconnects"
	MetricAMQP091EventDropped     = "AMQP091EventDropped"
)

// AMQP 1.0 metric names.
const (
	MetricAMQP10ConnectLatency   = "AMQP10ConnectLatency"
	MetricAMQP10ReconcileLatency = "AMQP10ReconcileLatency"
	MetricAMQP10SendLatency      = "AMQP10SendLatency"
	MetricAMQP10ReceiveLatency   = "AMQP10ReceiveLatency"
	MetricAMQP10AcceptLatency    = "AMQP10AcceptLatency"
	MetricAMQP10Reconnects       = "AMQP10Reconnects"
	MetricAMQP10EventDropped     = "AMQP10EventDropped"
)

// Standard dimension key names for metric tags.
const (
	TagKeyLeaseID   = "lease_id"
	TagKeyRouteID   = "route_id"
	TagKeySessionID = "session_id"
	TagKeyPartition = "partition"
	TagKeyQueueURL  = "queue_url"
	TagKeyCategory  = "category"
	TagKeyTransport = "transport"
	TagKeyEntity    = "entity"
)

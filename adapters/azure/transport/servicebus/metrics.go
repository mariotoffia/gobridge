package servicebus

// Metric names emitted by the Azure Service Bus transport adapter. Relocated
// from domain/shared as part of shared-kernel slimming; the string values are
// the wire identities reported to CloudWatch/OTel and MUST NOT change.
const (
	MetricASBReceiveLatency    = "ASBReceiveLatency"
	MetricASBCompleteLatency   = "ASBCompleteLatency"
	MetricASBSendLatency       = "ASBSendLatency"
	MetricASBSendBatchLatency  = "ASBSendBatchLatency"
	MetricASBScheduleLatency   = "ASBScheduleLatency"
	MetricASBLockRenewals      = "ASBLockRenewals"
	MetricASBMalformedMessages = "ASBMalformedMessages"
)

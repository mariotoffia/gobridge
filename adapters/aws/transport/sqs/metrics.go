package sqs

// Metric names emitted by the SQS transport adapter. Relocated from
// domain/shared as part of shared-kernel slimming; the string values are the
// wire identities reported to CloudWatch/OTel and MUST NOT change.
const (
	MetricSQSReceiveLatency       = "SQSReceiveLatency"
	MetricSQSDeleteLatency        = "SQSDeleteLatency"
	MetricSQSVisibilityExtensions = "SQSVisibilityExtensions"
	MetricSQSPollLatency          = "SQSPollLatency"
	MetricSQSSendLatency          = "SQSSendLatency"
	MetricSQSSendBatchLatency     = "SQSSendBatchLatency"
	MetricSQSAutoExtends          = "SQSAutoExtends"
	MetricSQSMalformedMessages    = "SQSMalformedMessages"
	// MetricSQSDroppedAttributes counts envelope headers dropped from a
	// send because they would have exceeded the SQS per-message attribute
	// count or size limits (Finding 11).
	MetricSQSDroppedAttributes = "SQSDroppedAttributes"
)

// TagKeyQueueURL is the dimension key used only by the SQS adapter.
const TagKeyQueueURL = "queue_url"

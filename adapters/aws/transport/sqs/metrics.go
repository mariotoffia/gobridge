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

	// Failure counters (Finding 5). The nine metrics above are all
	// latency/success; a poll, settlement (Ack/Retry) or auto-extend
	// failure was previously only a Warn log and thus metrics-invisible.
	// All three are tagged solely by TagKeyQueueURL (bounded cardinality),
	// matching the existing SQS metrics.
	//
	// MetricSQSPollErrors counts ReceiveMessage poll failures. Conversion
	// ("poison") failures are NOT counted here — pollAndConvert returns an
	// error only when ReceiveMessage itself fails; a per-message conversion
	// failure is counted by MetricSQSMalformedMessages instead.
	MetricSQSPollErrors = "SQSPollErrors"
	// MetricSQSSettlementErrors counts failed Ack (DeleteMessage) and Retry
	// (ChangeMessageVisibility) settlement calls.
	MetricSQSSettlementErrors = "SQSSettlementErrors"
	// MetricSQSAutoExtendFailures counts failed auto-extend
	// ChangeMessageVisibility calls in the background delivery loop.
	MetricSQSAutoExtendFailures = "SQSAutoExtendFailures"

	// MetricSQSMissingRedrivePolicy counts receiver startups whose
	// best-effort redrive-policy check ran and found the source queue has NO
	// native redrive policy (maxReceiveCount -> DLQ) (Chunk 13 HIGH-2).
	// Without a redrive policy a malformed ("poison") message the receiver
	// cannot convert would redeliver every visibility timeout forever, so the
	// absence is surfaced as a metric plus a startup warning. Emitted at most
	// once per receiver start, only when the check actually ran (a
	// permission-denied GetQueueAttributes does NOT emit it) and no adapter
	// poison backstop is configured. Tagged solely by TagKeyQueueURL.
	MetricSQSMissingRedrivePolicy = "SQSMissingRedrivePolicy"

	// MetricSQSPoisonDropped counts malformed ("poison") messages the adapter
	// DELETED as a backstop after ApproximateReceiveCount reached the
	// configured poison_max_receives (Chunk 13 HIGH-2). The delete is a
	// controlled, observable drop that breaks an otherwise-unbounded
	// redelivery hot loop on a source queue with no native redrive policy.
	// Tagged solely by TagKeyQueueURL.
	MetricSQSPoisonDropped = "SQSPoisonDropped"
)

// TagKeyQueueURL is the dimension key used only by the SQS adapter.
const TagKeyQueueURL = "queue_url"

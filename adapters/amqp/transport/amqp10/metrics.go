package amqp10

// Metric names emitted by the AMQP 1.0 transport adapter. Relocated from
// domain/shared as part of shared-kernel slimming; the string values are the
// wire identities reported to CloudWatch/OTel and MUST NOT change.
const (
	MetricAMQP10ConnectLatency   = "AMQP10ConnectLatency"
	MetricAMQP10ReconcileLatency = "AMQP10ReconcileLatency"
	MetricAMQP10SendLatency      = "AMQP10SendLatency"
	MetricAMQP10ReceiveLatency   = "AMQP10ReceiveLatency"
	MetricAMQP10AcceptLatency    = "AMQP10AcceptLatency"
	MetricAMQP10Reconnects       = "AMQP10Reconnects"
	MetricAMQP10EventDropped     = "AMQP10EventDropped"
	// MetricAMQP10DelayedRetryDeferred counts delayed (backoff) retries
	// whose redelivery timing was DEFERRED to the broker. AMQP 1.0 has no
	// portable client-side delayed-redelivery primitive, so a Retry with
	// a positive delay is handed back to the broker (ModifyMessage with
	// an x-opt-delivery-time annotation) and the broker decides when to
	// redeliver. On a broker that honors the annotation (e.g. Artemis)
	// the requested spacing IS applied; on one that ignores it the
	// spacing falls back to the broker's redelivery policy. This counter
	// therefore measures broker-delegated retry scheduling, NOT a failure
	// — the previous name ("Unhonored") asserted a non-honoring broker on
	// every delayed retry, a 100% false positive on honoring brokers
	// See acl_delivery.go Retry.
	MetricAMQP10DelayedRetryDeferred = "AMQP10DelayedRetryDeferred"
	// MetricAMQP10IngressRejected counts inbound messages that failed
	// envelope conversion at ingress and were rejected back to the
	// broker (poison messages). The receive loop keeps running — this
	// counter is the only signal that malformed input is arriving.
	MetricAMQP10IngressRejected = "AMQP10IngressRejected"
	// MetricAMQP10SettleFailed counts settlement attempts (Ack/Retry)
	// that FAILED on a live link. Each failure permanently consumes one
	// link-credit slot — go-amqp only replenishes credit on a completed
	// disposition — so an unobserved run of failures silently exhausts
	// credit and stalls the receiver with Health still Full. The receiver
	// forces a link rebuild once failures cross a threshold; this counter
	// makes the leaked-credit condition observable before then (finding
	// See receiver.go settlementFailed / forceSettleRebuild.
	MetricAMQP10SettleFailed = "AMQP10SettleFailed"
)

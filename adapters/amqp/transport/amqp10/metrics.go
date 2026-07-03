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
	// MetricAMQP10DelayedRetryUnhonored counts delayed (backoff) retries
	// whose requested spacing the client could not honor. AMQP 1.0 has no
	// portable client-side delayed-redelivery primitive, so a Retry with a
	// positive delay is handed back to the broker (ModifyMessage) and the
	// broker — not the bridge — decides when to redeliver. A climbing count
	// means the configured retry backoff is effectively broker-driven on this
	// transport (see acl_delivery.go Retry, finding 2 / G-N2).
	MetricAMQP10DelayedRetryUnhonored = "AMQP10DelayedRetryUnhonored"
	// MetricAMQP10IngressRejected counts inbound messages that failed
	// envelope conversion at ingress and were rejected back to the
	// broker (poison messages). The receive loop keeps running — this
	// counter is the only signal that malformed input is arriving.
	MetricAMQP10IngressRejected = "AMQP10IngressRejected"
)

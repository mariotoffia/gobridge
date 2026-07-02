package amqp091

// Metric names emitted by the AMQP 0-9-1 transport adapter. Relocated from
// domain/shared as part of shared-kernel slimming; the string values are the
// wire identities reported to CloudWatch/OTel and MUST NOT change.
const (
	MetricAMQP091ConnectLatency   = "AMQP091ConnectLatency"
	MetricAMQP091ReconcileLatency = "AMQP091ReconcileLatency"
	MetricAMQP091PublishLatency   = "AMQP091PublishLatency"
	MetricAMQP091ConsumeLatency   = "AMQP091ConsumeLatency"
	MetricAMQP091AckLatency       = "AMQP091AckLatency"
	MetricAMQP091Reconnects       = "AMQP091Reconnects"
	MetricAMQP091EventDropped     = "AMQP091EventDropped"
	// MetricAMQP091Blocked counts connection.blocked / connection.unblocked
	// transitions the broker raises under a resource alarm (memory/disk).
	// A non-zero, climbing count distinguishes broker pushback from
	// ordinary send timeouts.
	MetricAMQP091Blocked = "AMQP091Blocked"
)

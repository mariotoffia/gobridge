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
)

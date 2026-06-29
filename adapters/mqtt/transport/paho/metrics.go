package paho

// Metric names emitted by the MQTT (paho) transport adapter. Relocated from
// domain/shared as part of shared-kernel slimming; the string values are the
// wire identities reported to CloudWatch/OTel and MUST NOT change.
const (
	MetricMQTTPublishLatency   = "MQTTPublishLatency"
	MetricMQTTPublishFailures  = "MQTTPublishFailures"
	MetricMQTTHandlerPanics    = "MQTTHandlerPanics"
	MetricMQTTConnectLatency   = "MQTTConnectLatency"
	MetricMQTTReconcileLatency = "MQTTReconcileLatency"
	MetricMQTTRouterDropped    = "MQTTRouterDropped"
	MetricMQTTEventDropped     = "MQTTEventDropped"
)

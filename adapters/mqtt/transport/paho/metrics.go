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

	// MetricMQTTNonStringHeaderDropped counts bridge-to-bridge / application
	// header values dropped on egress because their value is not a string
	// and therefore cannot be serialised as an MQTT user property (e.g. a
	// non-string idempotency-key or tenant-id). Emitted by
	// PublishFromEnvelope so the otherwise-silent drop is observable.
	MetricMQTTNonStringHeaderDropped = "MQTTNonStringHeaderDropped"
)

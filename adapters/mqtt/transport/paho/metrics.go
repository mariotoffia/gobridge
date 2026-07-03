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

	// MetricMQTTRouterBuffered counts publishes that arrived before any
	// matching handler registered (e.g. the persistent-session backlog
	// delivered on CONNACK before Receiver.Run runs) and were held in the
	// router's bounded pending buffer instead of being dropped.
	MetricMQTTRouterBuffered = "MQTTRouterBuffered"

	// MetricMQTTSessionTakeover counts server disconnects with reason code
	// 0x8E/0x8F (session taken over): another client connected with the
	// same ClientID. A steadily increasing count signals two instances
	// sharing a client_id and mutually kicking each other.
	MetricMQTTSessionTakeover = "MQTTSessionTakeover"
)

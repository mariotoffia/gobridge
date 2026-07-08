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

	// MetricMQTTRouterUnmatchedDropped counts publishes that matched NO
	// registered topic filter AFTER the startup grace window elapsed AND
	// whose topic is NOT covered by any subscription the session still
	// wants — the signature of an ORPHAN broker-side subscription (a route
	// removed from config whose subscription survives on the resumed
	// clean_start=false session). Such a publish is acked-and-dropped so its
	// un-acked slot no longer pins the broker's Receive-Maximum in-flight
	// window and no longer head-of-line-blocks in-order acking for the rest
	// of the session. This is BENIGN cleanup. Real live-route loss (a
	// still-desired subscription whose handler registered late) is counted
	// separately on MetricMQTTRouterCoveredDropped. A steadily rising count
	// means the broker is still delivering for a subscription no configured
	// route covers; see the router's unsubscribe-on-resume hygiene
	// (acl_router.go, doc.go).
	MetricMQTTRouterUnmatchedDropped = "MQTTRouterUnmatchedDropped"

	// MetricMQTTRouterCoveredDropped counts publishes acked-and-dropped past
	// the startup grace window on a topic STILL covered by a subscription the
	// session wants (an active broker subscription or a desired plan filter)
	// whose receiver handler had not registered in time. Unlike an orphan
	// drop (MetricMQTTRouterUnmatchedDropped) this is REAL message loss on a
	// live route: the broker already acked the publish to the client, so it
	// cannot be recovered, and dropping-with-ack is the only way to keep
	// paho's in-order ack stream draining. ANY non-zero value is alarming —
	// it means a receiver handler registered later than unmatched_grace
	// (30s default) and lost data. Split out from
	// MetricMQTTRouterUnmatchedDropped so this real loss is not masked by
	// benign orphan cleanup.
	MetricMQTTRouterCoveredDropped = "MQTTRouterCoveredDropped"

	// MetricMQTTRouterOverflowDropped counts QoS 1/2 publishes acked-and-dropped
	// because the startup pending buffer was FULL (entry or byte cap) during the
	// unmatched-grace window and no evictable QoS 0 entry was available. This is
	// REAL message loss: the broker already delivered the publish, and dropping
	// it forfeits redelivery — the ack is issued ONLY to keep paho's in-order
	// ack stream draining (an un-acked overflow slot would head-of-line-block
	// every later ack, pin the broker's Receive-Maximum window, and deadlock
	// ingress on a live connection). ANY non-zero value is alarming. Kept
	// DISTINCT from the generic MetricMQTTRouterDropped (QoS 0 best-effort
	// overflow, no delivery contract) and from the covered/orphan past-grace
	// drops (MetricMQTTRouterCoveredDropped / MetricMQTTRouterUnmatchedDropped)
	// so this real QoS 1/2 startup-overflow loss is never masked by a benign
	// best-effort drop (F-2 / M-1).
	MetricMQTTRouterOverflowDropped = "MQTTRouterOverflowDropped"

	// MetricMQTTSessionTakeover counts server disconnects with reason code
	// 0x8E (session taken over): another client connected with the same
	// ClientID. A steadily increasing count signals two instances sharing a
	// client_id and mutually kicking each other. (0x8F is Topic Filter
	// Invalid — a different condition — and is NOT counted here.)
	MetricMQTTSessionTakeover = "MQTTSessionTakeover"
)

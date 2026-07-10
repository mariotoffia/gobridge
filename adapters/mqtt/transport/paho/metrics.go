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
	// of the session. This is BENIGN cleanup. A still-desired subscription
	// whose handler registered late is NOT counted here and is NOT lost: its
	// QoS 1/2 is RETAINED un-acked (MetricMQTTRouterCoveredRetained) and only a
	// covered QoS 0 that ALSO overflows the bounded buffer is dropped
	// (MetricMQTTRouterCoveredDropped). A steadily rising count
	// means the broker is still delivering for a subscription no configured
	// route covers; see the router's unsubscribe-on-resume hygiene
	// (acl_router.go, doc.go).
	MetricMQTTRouterUnmatchedDropped = "MQTTRouterUnmatchedDropped"

	// MetricMQTTRouterCoveredDropped counts QoS 0 publishes dropped past the
	// startup grace window on a topic STILL covered by a subscription the
	// session wants (an active broker subscription or a desired plan filter)
	// whose receiver handler had not registered in time AND which the bounded
	// pending buffer could not hold (its QoS 0 count/byte ceiling was full).
	// QoS 0 carries no redelivery contract, so a covered QoS 0 the buffer
	// cannot retain is a best-effort loss. Covered QoS 1/2 is NEVER counted
	// here: it is RETAINED un-acked instead (MetricMQTTRouterCoveredRetained)
	// so at-least-once holds — dropping a covered live-route QoS 1/2 would be
	// acknowledged loss (HIGH-1). ANY non-zero value means a receiver handler
	// registered later than unmatched_grace (30s default) while a covered QoS 0
	// backlog overflowed the buffer. Split out from
	// MetricMQTTRouterUnmatchedDropped so this loss is not masked by benign
	// orphan cleanup.
	MetricMQTTRouterCoveredDropped = "MQTTRouterCoveredDropped"

	// MetricMQTTRouterCoveredRetained counts publishes on a STILL-COVERED topic
	// RETAINED un-acked past the startup grace window because their receiver
	// handler had not registered yet (HIGH-1). Unlike MetricMQTTRouterCoveredDropped
	// these are NOT lost: rather than ack-and-drop a still-desired live-route
	// publish (which would convert startup slowness into acknowledged loss and
	// break at-least-once), the router keeps it in the bounded pending buffer
	// (bounded by receive_maximum) until the handler registers and flushes it —
	// or the broker redelivers it on reconnect. A steadily rising count means a
	// receiver is registering later than unmatched_grace (30s default) or never;
	// investigate the slow/absent receiver, but no data is lost. Retained QoS 1/2
	// pins a broker Receive-Maximum slot until settled, so a persistent backlog
	// eventually applies ingress backpressure rather than dropping.
	MetricMQTTRouterCoveredRetained = "MQTTRouterCoveredRetained"

	// MetricMQTTRouterOverflowDropped counts QoS 1/2 publishes acked-and-dropped
	// because the startup pending buffer's COUNT cap (== receive_maximum) was
	// hit with NO evictable QoS 0 to reclaim. This is UNREACHABLE under a
	// spec-compliant broker: Receive-Maximum flow control caps in-flight
	// (un-acked) QoS 1/2 at the count cap, so the buffer can never overflow with
	// QoS 1/2 alone. The independent 64 MiB byte ceiling NEVER drops QoS 1/2 (it
	// governs QoS 0 memory only). Therefore ANY non-zero value means a broker
	// delivered more un-acked QoS 1/2 than the Receive Maximum it was granted —
	// a protocol violation. The victim is acked-and-dropped (NOT left un-acked:
	// an un-acked drop would head-of-line-block paho's contiguous-prefix ack
	// stream and wedge ingress). Kept DISTINCT from the generic
	// MetricMQTTRouterDropped (QoS 0 best-effort overflow) and from the
	// covered/orphan past-grace drops (MetricMQTTRouterCoveredDropped /
	// MetricMQTTRouterUnmatchedDropped) so this protocol-violation loss is never
	// masked (c4-qos12-overflow / F-2 / M-1).
	MetricMQTTRouterOverflowDropped = "MQTTRouterOverflowDropped"

	// MetricMQTTSessionTakeover counts server disconnects with reason code
	// 0x8E (session taken over): another client connected with the same
	// ClientID. A steadily increasing count signals two instances sharing a
	// client_id and mutually kicking each other. (0x8F is Topic Filter
	// Invalid — a different condition — and is NOT counted here.)
	MetricMQTTSessionTakeover = "MQTTSessionTakeover"

	// MetricMQTTQoSDowngraded counts subscriptions the broker accepted at a
	// LOWER QoS than requested (a granted-QoS downgrade: e.g. requested QoS 2,
	// SUBACK reason 0x00 = granted QoS 0). The route assumes the requested
	// delivery guarantee, so a silent downgrade quietly removes offline /
	// redelivery guarantees and opens a disconnect-gap loss window. The
	// reconcile loop stores the REQUESTED QoS in activeSubs (its delta baseline,
	// so a stable downgraded sub is not re-subscribed every cycle) and
	// increments this counter with a loud warning ONCE per subscription
	// transition — initial subscribe, reconnect, or a plan that changes the
	// requested QoS — rather than on every reconcile (c4-qos-downgrade). ANY
	// non-zero value warrants investigating a broker QoS-cap policy.
	MetricMQTTQoSDowngraded = "MQTTQoSDowngraded"
)

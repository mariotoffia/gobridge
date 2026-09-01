package paho

// Metric names emitted by the MQTT (paho) transport adapter. Relocated from
// domain/shared as part of shared-kernel slimming; the string values are the
// wire identities reported to CloudWatch/OTel and MUST NOT change.
const (
	MetricMQTTPublishLatency           = "MQTTPublishLatency"
	MetricMQTTPublishFailures          = "MQTTPublishFailures"
	MetricMQTTHandlerPanics            = "MQTTHandlerPanics"
	MetricMQTTConnectLatency           = "MQTTConnectLatency"
	MetricMQTTReconcileLatency         = "MQTTReconcileLatency"
	MetricMQTTRouterDropped            = "MQTTRouterDropped"
	MetricMQTTEventDropped             = "MQTTEventDropped"
	MetricMQTTSessionRecoveryRecycle   = "MQTTSessionRecoveryRecycle"
	MetricMQTTUnsettled                = "MQTTUnsettled"
	MetricMQTTOldestUnsettledAge       = "MQTTOldestUnsettledAge"
	MetricMQTTReceiveWindowUtilization = "MQTTReceiveWindowUtilization"

	// MetricMQTTNonStringHeaderDropped counts header values dropped on egress
	// because they cannot be represented on the wire: a bridge-to-bridge /
	// application value that is not a string and therefore cannot be serialised
	// as an MQTT user property (e.g. a non-string idempotency-key or
	// tenant-id), or a retained binary correlation header whose encoding no
	// longer decodes. Emitted by PublishFromEnvelope so the otherwise-silent
	// drop is observable.
	MetricMQTTNonStringHeaderDropped = "MQTTNonStringHeaderDropped"

	// MetricMQTTEgressRejected counts publishes refused BEFORE any byte reached
	// the socket because the constructed packet violates a wire limit: a
	// length-prefixed field above the MQTT v5 65,535-byte ceiling (which Paho
	// would silently truncate, so the broker would acknowledge metadata that
	// differs from the source), or an encoded packet larger than the Maximum
	// Packet Size the broker granted in its CONNACK (which the broker answers
	// with a DISCONNECT, leaving QoS 1/2 completion ambiguous and churning the
	// session on every retry). The publish is returned to the route as a
	// permanent rejection, so it is DLQ'd rather than retried. Any non-zero
	// value means a producer or a route is generating messages this broker
	// cannot accept — find the oversized field or lower the message size before
	// the route's DLQ fills.
	MetricMQTTEgressRejected = "MQTTEgressRejected"

	// MetricMQTTIngressHeaderDropped counts inbound MQTT user properties
	// dropped on ingress because their key or value is unsafe (not valid
	// UTF-8, or contains a control character) or exceeds maxHeaderValueLen.
	// It is the ingress counterpart to MetricMQTTNonStringHeaderDropped:
	// without it a peer publishing a spec-legal-but-rejected header (e.g. an
	// over-long value) loses it silently, and a route filtering on that
	// header misroutes with nothing to debug from. Reserved and
	// adapter-controlled keys stripped on purpose are NOT counted here — only
	// application/bridge user properties lost to the safety filter.
	MetricMQTTIngressHeaderDropped = "MQTTIngressHeaderDropped"

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
	// acknowledged loss. ANY non-zero value means a receiver handler
	// registered later than unmatched_grace (30s default) while a covered QoS 0
	// backlog overflowed the buffer. Split out from
	// MetricMQTTRouterUnmatchedDropped so this loss is not masked by benign
	// orphan cleanup.
	MetricMQTTRouterCoveredDropped = "MQTTRouterCoveredDropped"

	// MetricMQTTRouterCoveredRetained counts publishes on a STILL-COVERED topic
	// RETAINED un-acked past the startup grace window because their receiver
	// handler had not registered yet. Unlike MetricMQTTRouterCoveredDropped
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
	// masked (c4-qos12-overflow /).
	MetricMQTTRouterOverflowDropped = "MQTTRouterOverflowDropped"

	// MetricMQTTRouterStalePurged counts publishes DISCARDED because they
	// belong to a PRIOR broker connection generation. Two branches feed it:
	//
	//   - Reconnect purge: pre-registration pending publishes buffered
	//     under a previous connection are purged on reconnect. Their protocol
	//     acks died with the old connection (paho ErrPacketNotFound), and a
	//     clean_start=false broker REDELIVERS every un-acked QoS 1/2 with
	//     FRESH packet IDs — keeping the stale twins would let a redelivered
	//     copy pile up beside its ghost until the count cap (==
	//     receive_maximum) ack-drops a LIVE message as a bogus
	//     MetricMQTTRouterOverflowDropped, breaking at-least-once.
	//   - Recycle-window discard: publishes still arriving on (or
	//     already queued from) the OLD socket while a recovery/managed-cleanup
	//     recycle is disconnecting it are released without dispatch or ack.
	//     Previously this branch was the router's only fully silent drop.
	//
	// It also counts ingress released while the session is CLOSING: the router
	// is stopped before the SDK disconnect (otherwise a parked publish callback
	// pins that disconnect for the whole close deadline), so publishes keep
	// arriving, and anything still queued for dispatch is shed.
	//
	// In EVERY branch QoS 1/2 entries are NOT lost (the resumed session
	// redelivers them); QoS 0 entries are a best-effort loss (no redelivery
	// contract across a disconnect, by protocol) and are counted on
	// MetricMQTTRouterDropped instead. A steadily rising count means frequent
	// reconnects/recycles/closes while traffic is in flight — expected churn,
	// not data loss for QoS 1/2.
	MetricMQTTRouterStalePurged = "MQTTRouterStalePurged"

	// MetricMQTTSessionTakeover counts server disconnects with reason code
	// 0x8E (session taken over): another client connected with the same
	// ClientID. A steadily increasing count signals two instances sharing a
	// client_id and mutually kicking each other. (0x8F is Topic Filter
	// Invalid — a different condition — and is NOT counted here.)
	MetricMQTTSessionTakeover = "MQTTSessionTakeover"

	// MetricMQTTIngressPoisonDropped counts inbound publishes ACKED-AND-DROPPED
	// because they violate a LOCAL representational cap the broker cannot
	// enforce — max_payload_bytes, the ingress metadata byte cap, or the
	// ingress User Property count cap — while fitting inside the CONNECT-
	// advertised Maximum Packet Size (the only inbound limit a compliant
	// broker enforces). Any authorized publisher can produce such a packet, so
	// terminating the session on it would hand every publisher a permanent
	// kill switch: the un-acked packet would be redelivered on each
	// clean_start=false resume and re-latch the session terminal forever
	// Instead the packet is acked (freeing the broker's in-flight
	// slot and stopping redelivery) and dropped, and this counter is the
	// deliberate-loss record. ANY non-zero value means a publisher is sending
	// packets this bridge is configured to refuse — alert on it and find the
	// publisher (see docs/runbooks/mqtt-ingress-poison.md). Only violations a
	// broker could never forward (malformed packets, total size above the
	// advertised Maximum Packet Size) still fail the session closed.
	MetricMQTTIngressPoisonDropped = "MQTTIngressPoisonDropped"

	// MetricMQTTReceiverEmitRejected counts inbound deliveries the route
	// pipeline REFUSED at emit — a shutting-down or wedged route runner. It is
	// tagged with the session and an outcome:
	//
	//   - "recovering": the delivery was durable (QoS 1/2), so it is left
	//     un-acked and a bounded session recycle makes the broker redeliver it.
	//     The count is the leading indicator of the recycle that follows.
	//   - "lost": the delivery was QoS 0 — no acknowledgement to withhold and no
	//     redelivery contract, so the message is gone. Any non-zero "lost" count
	//     is acknowledged best-effort loss; alert on it if the deployment treats
	//     QoS 0 ingress as significant.
	MetricMQTTReceiverEmitRejected = "MQTTReceiverEmitRejected"

	// MetricMQTTAckAfterReconnect counts delivery settlements whose protocol
	// acknowledgement could not reach the broker because the connection was torn
	// down and re-established between receive and settle. It is measured from
	// the connection generation captured at receive, NOT from an SDK error
	// class: paho marks an acknowledgement and flushes the acknowledged prefix
	// asynchronously, so an ack marked just before the connection dropped
	// returns success and is still redelivered. The settlement reports SUCCESS
	// to the runtime
	// — the broker redelivers the packet on the resumed session and downstream
	// idempotency/dedup absorbs the duplicate (documented at-least-once
	// residual) — but each count here is a GUARANTEED broker
	// redelivery: a burst after a reconnect storm is the leading indicator of
	// a duplicate flood on routes without downstream dedup (direct_hold).
	MetricMQTTAckAfterReconnect = "MQTTAckAfterReconnect"

	// MetricMQTTConnectFailures counts rejected or failed CONNECT attempts,
	// tagged with the session and the bounded BridgeError code the failure
	// classified to (UNAVAILABLE, NOT_AUTHORIZED, TIMEOUT, CONNECTION_LOST, …).
	// MQTT authenticates only at CONNECT and autopaho then retries forever
	// behind the scenes, so this is the ONE place a reconnect failure names its
	// cause; the same cause is latched on SessionHealth.LastError until the
	// session is up again. The broker URL is deliberately NOT a dimension (it
	// may carry credentials); only the bounded code is. A rising NOT_AUTHORIZED
	// rate is a credential problem (see the credential-expiry runbook); a
	// rising UNAVAILABLE / CONNECTION_LOST rate is a broker or network outage.
	MetricMQTTConnectFailures = "MQTTConnectFailures"

	// MetricMQTTSessionResumeLost counts connections where a persistent or
	// exclusive session asked the broker to RESUME (clean_start=false) and the
	// CONNACK answered Session Present=false: the broker had no session state,
	// so the subscriptions and the queued offline QoS 1/2 backlog for this
	// client id are gone. Causes are a session expiry elapsed during a long
	// outage, a broker restart without persistence, or an exclusive standby
	// connecting after session_expiry_interval. Re-subscribing then succeeds
	// and the session reports healthy again, so WITHOUT this counter the
	// discontinuity is invisible. Tagged with the session. Any non-zero value
	// means offline continuity — the reason those modes exist — was broken at
	// least once; see docs/runbooks/broker-outage-reconnect-storm.md.
	MetricMQTTSessionResumeLost = "MQTTSessionResumeLost"

	// MetricMQTTQoSDowngraded counts subscriptions the broker granted at a
	// LOWER QoS than requested (for example, requested QoS 2 and SUBACK reason
	// 0x00 granting QoS 0). Reconcile emits a loud warning, leaves the filter
	// inactive, and returns ErrQoSNotSupported with topic, requested QoS, and
	// granted QoS context, so readiness remains non-Full. The broker-observed
	// grant suppresses an unchanged immediate re-subscribe; this counter advances
	// only when a broker SUBACK newly reports the downgrade. Any non-zero value
	// warrants investigating a broker QoS-cap policy.
	MetricMQTTQoSDowngraded = "MQTTQoSDowngraded"
)

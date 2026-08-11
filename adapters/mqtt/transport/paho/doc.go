// Package paho implements ports.Session, ports.Receiver, and ports.Sender
// for MQTT using the Eclipse Paho Go v5 client library with autopaho for
// automatic reconnection.
//
// Session owns the broker connection, ClientID identity, clean start /
// session expiry configuration, subscription reconciliation, and in-flight
// QoS 1/2 protocol state. Receiver and Sender are thin wrappers that
// delegate to the Session's underlying ConnectionManager.
//
// MQTT 5 is the primary target; MQTT 3.1.1 degrades gracefully with
// startup warnings for unavailable features.
//
// Session modes: Ephemeral, Persistent, Exclusive.
//
// Key design decisions:
//
//   - Manual protocol acknowledgment (ack-after-durable-handoff): the Paho
//     client runs with EnableManualAcknowledgment, so the PUBACK (QoS 1) /
//     PUBCOMP (QoS 2) is sent only when the runtime SETTLES the delivery
//     via Delivery.Ack — after outbox persist or pipeline completion — not
//     when the message handler returns. A crash with N messages in flight
//     leaves them un-acked on the broker; a persistent/exclusive session
//     redelivers them to the next owner. The Paho client serialises acks
//     in receive order internally, so concurrent out-of-order settlement
//     is safe (head-of-line: acks queue behind the oldest unsettled
//     message). See delivery.go for the full contract and residual
//     boundaries.
//
//   - No local message drops under load: the router dispatches each inbound
//     publish SYNCHRONOUSLY — the publish callback blocks on the emit
//     callback — so a slow downstream fills the broker's Receive Maximum
//     window (un-acked QoS 1/2 messages) and stops read-ahead instead of
//     spawning unbounded goroutines (backpressure).
//
//   - Startup buffering, then covered-retain / orphan ack-and-drop:
//     publishes arriving before a matching Receiver registers
//     (persistent-session backlog delivered on CONNACK before route
//     runners start) are held un-acked in a bounded buffer during a
//     startup GRACE WINDOW (session unmatched_grace, default 30s,
//     restarted on every (re)connect) and flushed, in order, when the
//     handler registers — never silently acked-and-dropped. AFTER the
//     window a still-unmatched publish is classified by whether a
//     subscription the session still wants COVERS its topic:
//
//     COVERED (an active broker subscription, a filter in the current
//     plan, or pending managed history — a live route whose receiver
//     handler registered late): the publish is RETAINED un-acked in the
//     bounded pending buffer (MetricMQTTRouterCoveredRetained) until the
//     handler registers and flushes it, or the broker redelivers it on
//     reconnect. It is NEVER acked-and-dropped — that would convert
//     startup slowness into acknowledged live-route loss and break
//     at-least-once. Only a covered QoS 0 the buffer cannot hold is a
//     best-effort drop (MetricMQTTRouterCoveredDropped). Before the FIRST
//     Reconcile of a process lifetime EVERY topic is treated as covered,
//     so a broker backlog replayed on CONNACK ahead of the first plan can
//     never be misclassified as orphan traffic.
//
//     ORPHAN (no subscription still wants the topic — a route removed
//     from config whose subscription survives on the resumed
//     clean_start=false session): it is acked-and-dropped
//     (MetricMQTTRouterUnmatchedDropped) so its un-acked slot stops
//     pinning the broker's Receive-Maximum in-flight window and
//     head-of-line-blocking in-order acks for the whole session, and its
//     exact topic is unsubscribed to converge broker state (see
//     acl_router_grace.go). Without this, one permanently-un-acked
//     orphan publish stalls ingress for every route on the shared session.
//     Unsubscribe-on-resume hygiene: MQTT offers no way to list a
//     session's server-side subscriptions, so an orphan identifies itself
//     by its own post-grace publish and is unsubscribed by that exact
//     topic (deduped per topic, one warn + one unsubscribe). A wildcard
//     orphan subscription may survive the concrete-topic UNSUBSCRIBE, but
//     its publishes keep being acked-and-dropped, so the stall cannot
//     recur; configure the managed subscription store to converge
//     wildcard filters durably.
//
//   - Ingress poison escape (never a session kill switch): an inbound
//     publish that violates a LOCAL representational cap the broker
//     cannot enforce (max_payload_bytes, the metadata byte cap, the User
//     Property count cap) while fitting the CONNECT-advertised Maximum
//     Packet Size is ACKED-and-DROPPED
//     (MetricMQTTIngressPoisonDropped) — a deliberate, loudly counted
//     loss. Terminating the session instead would hand any authorized
//     publisher a permanent kill switch: the un-acked packet would be
//     redelivered on every clean_start=false resume and re-latch the
//     session terminal forever. Only violations a compliant
//     broker can never forward (malformed packets, total size above the
//     advertised maximum) fail the session closed, at the raw pre-decode
//     guard (ingress_conn.go).
//
//   - Per-receiver topic filtering: each Receiver registers the MQTT topic
//     filters of its subscriptions (wildcards + and # supported); a
//     publish is dispatched only to receivers whose filters match, so
//     routes sharing one session do not process each other's traffic.
//
//   - Header injection prevention: reserved x-bridge.* headers are stripped
//     from incoming MQTT messages at ingress; on egress, INTERNAL-ONLY
//     reserved headers are stripped so bridge bookkeeping does not leak to
//     non-bridge subscribers (see acl_headers.go).
//
//   - QoS completion: Sender.Send blocks until PUBACK (QoS 1) or PUBCOMP
//     (QoS 2), so a nil return confirms broker acceptance.
//
//   - QoS/retain are broker<->client packet semantics, not end-to-end
//     guarantees.
//
// ponytail: QoS 0 flood ceiling. QoS 0 publishes are not bounded by
// Receive Maximum — the broker may push them faster than downstream
// settles. Because dispatch is synchronous, a QoS 0 flood fills the Paho
// client's internal publish channel and can stall its incoming loop
// (delaying PINGRESP / PUBACK processing) until downstream catches up;
// tuning receive_maximum low makes the QoS 1/2 window smaller but does
// NOT throttle QoS 0. This is the deliberate trade-off for not buffering
// unboundedly in memory: prefer QoS 1 for traffic that matters, and keep
// QoS 0 flows behind a downstream that keeps up.
package paho

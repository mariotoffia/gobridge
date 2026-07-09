// Package amqp10 implements ports.Session, ports.Receiver, ports.Sender,
// and ports.BatchSender for AMQP 1.0 (OASIS standard) brokers. The
// adapter is exercised in CI against Apache ActiveMQ Artemis; any broker
// that conforms to the AMQP 1.0 wire protocol is expected to
// interoperate, but only Artemis is covered by the integration suite.
//
// # Architecture
//
// AMQP 1.0 is treated as a stateful session transport. The Session type
// owns an *amqp.Conn and *amqp.Session, providing automatic reconnection
// with exponential backoff. Receiver and Sender create their own AMQP
// links (receiver/sender) on the session's underlying AMQP session,
// re-creating links on detach or connection loss.
//
// # Settlement Mapping
//
// AMQP 1.0 uses explicit message disposition for settlement:
//
//   - Ack:            AcceptMessage   (disposition: accepted)
//   - Retry(after=0): ReleaseMessage  (make available for immediate redelivery)
//   - Retry(after>0): ModifyMessage   (DeliveryFailed=true; hands the message
//     back to the broker, which controls redelivery timing — the requested
//     delay is advisory only, never dropped, see Delivery.Retry)
//   - Extend:         ErrNotSupported (AMQP 1.0 uses credit-based flow control,
//     not visibility timeouts)
//
// # Delayed Retry Requires Broker-Side redelivery-delay
//
// A delayed Retry attaches the x-opt-delivery-time annotation via the
// modified outcome, but the BROKER owns redelivery timing. On brokers
// that do not honor the annotation for redeliveries, a broker-side
// redelivery delay is MANDATORY for the runtime's retry backoff to have
// any effect: ActiveMQ Artemis defaults redelivery-delay to 0, so
// without address-settings configuring redelivery-delay (and ideally
// redelivery-delay-multiplier/max-redelivery-delay) every delayed retry
// is redelivered immediately and max-delivery-attempts can be exhausted
// in milliseconds. Each delayed retry deferred to broker scheduling
// increments MetricAMQP10DelayedRetryDeferred and warns once per link.
//
// # Subject vs Address
//
// AMQP 1.0 sender links are address-bound: the link's target is fixed
// at link creation. ports.OutboundMessage.Address is therefore
// validated against the configured sender link address. An empty
// Address selects the configured address; a non-empty Address that
// does not match yields shared.ErrInvalidTopic without contacting the
// broker. On egress, Envelope.Subject is the SOLE source for the AMQP
// Properties.Subject: the amqp10.subject header is informational only
// and never sets Subject, so it cannot override or spoof Subject when
// Envelope.Subject is empty. On receive, Properties.Subject populates
// Envelope.Subject; a missing Properties.Subject leaves Envelope.Subject
// empty — there is no fallback to the link address.
//
// # Header Mapping
//
// Standard AMQP 1.0 message properties (message-id, correlation-id,
// content-type, subject, to, reply-to, etc.) are mapped to envelope
// headers with the "amqp10." prefix. Application properties are mapped
// directly as envelope headers. Reserved bridge headers (x-bridge.*)
// are stripped from incoming messages at ingress. On egress the central
// header policy applies: INTERNAL-ONLY reserved headers (and any
// unclassified x-bridge.* keys) are stripped, while BRIDGE-TO-BRIDGE
// PROPAGATED headers (correlation-id, idempotency-key, ordering-key,
// tenant-id, traceparent, ...) pass through as application properties so
// a peer bridge can correlate, deduplicate and continue a trace.
//
// # Error Mapping
//
// AMQP 1.0 error conditions (amqp:not-found, amqp:unauthorized-access,
// amqp:connection:forced, etc.) are classified into shared.BridgeError
// categories:
//
//   - amqp:not-found               -> ErrNotFound        (permanent)
//   - amqp:unauthorized-access     -> ErrNotAuthorized    (permanent)
//   - amqp:not-allowed             -> ErrForbidden        (permanent)
//   - amqp:resource-limit-exceeded -> ErrThrottled        (transient)
//   - amqp:connection:forced       -> ErrConnectionLost   (transient)
//   - amqp:link:detach-forced      -> ErrConnectionLost   (transient)
//   - amqp:link:message-size-exceeded -> ErrPayloadTooLarge (rejected)
//   - context.DeadlineExceeded     -> ErrTimeout          (transient)
//
// # Connection & High Availability
//
// SessionOptions.Address is a SINGLE broker endpoint. Reconnection
// re-dials that same endpoint with exponential backoff; the adapter does
// not maintain a client-side list of broker URLs and performs no
// multi-broker failover. For highly-available deployments, place the
// brokers behind an external load balancer or a DNS name that resolves
// to a healthy node (e.g. a VIP, an Artemis cluster connector, or a
// service-mesh address) so a single Address transparently follows
// failover. ponytail: client-side endpoint-list failover is deliberately
// out of scope — HA is delegated to the network/broker layer.
//
// Half-open detection: the connection monitor does NOT probe a live
// connection, so a SILENT half-open drop (SIGKILL / blackhole / NAT
// eviction with no TCP FIN) is surfaced only by SessionOptions.IdleTimeout
// — go-amqp arms it as the connection read deadline. The default is
// therefore an HA-oriented 30s (see defaultIdleTimeout) so half-open
// detection plus standby reattach meets the 30-60s failover target; a
// longer value lags reattach. The tradeoff is more keepalive traffic and
// that a network stall longer than the timeout drops an otherwise-healthy
// connection (the reconnect loop re-establishes it); operators on a
// lossy/high-latency link may raise IdleTimeout explicitly.
//
// # Durable Subscriptions & the Dedicated-Session Contract
//
// CONTRACT: a durable receiver (durability_mode > 0) MUST be placed on its
// OWN dedicated Session (its own session_id) — do not multiplex a durable
// receiver on the same session as other live receivers or senders.
//
// Why: one Session owns exactly one AMQP connection and multiplexes ALL of
// its receivers and senders over it. Closing a durable receiver cannot use
// a normal link detach, because the pinned go-amqp (v1.5.1) can only emit a
// CLOSING detach (Detach{Closed:true}); Artemis reads that as UNSUBSCRIBE
// and DESTROYS the durable terminus (dropping every retained message). The
// only way to detach the live durable link while PRESERVING the durable
// subscription is to drop the whole connection — a non-closing detach of
// every link on it (see Receiver.closeLink, c7-durable-close).
//
// Consequence (blast radius): closing a durable receiver forces a full
// connection teardown, which transiently blips EVERY sibling link on the
// same Session — in-flight sender publishes must relatch and non-durable
// receivers redeliver. The reconnect loop re-establishes the connection and
// siblings resume, so recovery is BOUNDED, but the disruption is real.
// Isolating durable receivers on a dedicated session confines a durable
// close to that session and keeps unrelated traffic untouched. If go-amqp
// ever supports a non-closing detach (or an Artemis unsubscribe-vs-detach
// distinction), the connection teardown can be narrowed to a link detach.
//
// # Credentials on the Wire
//
// SASL PLAIN transmits the username/password in cleartext frames. Config
// validation therefore REJECTS PLAIN (explicit sasl_mechanism=plain, the
// inferred default when a username is present, or credentials embedded in
// the Address URL — go-amqp selects PLAIN from userinfo) over a non-TLS
// scheme by default (c7-plain-plaintext, fail-closed). Use amqps:// /
// amqp+ssl://, switch to SASL EXTERNAL (mTLS), or set
// allow_insecure_plain=true to opt into the insecure path on a trusted
// network as a deliberate, auditable choice.
//
// The gate holds on EVERY credential-injection path, not just the build
// boundary: config/factory construction, Config.ApplyCredentials, AND the
// runtime rotation path Session.ApplyCredentials. A live rotation that
// would newly expose PLAIN over a non-TLS scheme is REFUSED and the
// session keeps its last-good credentials (no cleartext re-dial) — so a
// session that started without a username cannot later be rotated into a
// cleartext credential leak.
package amqp10

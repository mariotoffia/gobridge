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
// in milliseconds. Each unhonorable delay increments
// MetricAMQP10DelayedRetryUnhonored and warns once per link.
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
package amqp10

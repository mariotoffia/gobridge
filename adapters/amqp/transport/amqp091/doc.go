// Package amqp091 implements ports.Session, ports.Receiver, ports.Sender,
// and ports.BatchSender for AMQP 0-9-1 (RabbitMQ) using the
// github.com/rabbitmq/amqp091-go client library.
//
// # Architecture
//
// AMQP 0-9-1 is treated as a stateful session transport. A Session owns
// a single *amqp091.Connection, handles automatic reconnection with
// exponential backoff, and declares exchanges/queues/bindings during
// Reconcile. Receiver and Sender obtain AMQP channels from the Session's
// connection.
//
// # Settlement Mapping
//
//   - Ack:    delivery.Ack (single-message acknowledgement)
//   - Retry:  delivery.Nack with requeue=true (immediate redelivery)
//   - Extend: ErrNotSupported (AMQP 0-9-1 has no visibility timeout)
//
// At-least-once is the default and only managed mode: a managed receiver
// settles each delivery after the downstream step succeeds. Broker
// auto-ack (receiver.auto_ack=true) is rejected by Config.Validate and by
// the receiver factory because it acknowledges on delivery and loses
// messages on a downstream failure. The low-level NewReceiver still
// honours ReceiverConfig.AutoAck for manual/embedded use.
//
// receiver.prefetch_count defaults to a bounded value (see
// defaultPrefetchCount) on every path. A zero/omitted prefetch is treated
// as that default rather than "unlimited", so a manual-settlement
// consumer cannot be handed the whole queue at once.
//
// # Publishing
//
// Senders publish with publisher confirms enabled and wait for the
// per-message confirm, so a Send only reports success once the broker has
// accepted the message. The AMQP basic.publish "immediate" flag is not
// supported: RabbitMQ removed it in 3.0 (it closes the channel), so
// sender.immediate=true is rejected by Config.Validate and the sender
// factory, and the publish path never sets it.
//
// # Connection Flow Control
//
// The Session observes connection.blocked / connection.unblocked
// notifications (RabbitMQ resource alarms — memory/disk high watermark).
// While blocked, Health reports ServiceLevelDegraded with a
// shared.ErrBrokerBusy LastError and the AMQP091Blocked metric is
// emitted, so a stalled-by-backpressure broker is distinguishable from a
// run of send timeouts. The connection stays Ready (the route is intact;
// only publishing is paused).
//
// # Header Mapping
//
// AMQP 0-9-1 system properties (MessageId, CorrelationId, ContentType,
// ContentEncoding, ReplyTo, Type, AppId, DeliveryMode, Priority,
// Expiration, Timestamp) are mapped to envelope headers with the
// "amqp091." prefix. User-defined headers from the AMQP Headers table
// are mapped directly.
//
// Header policy is asymmetric:
//   - INGRESS: all reserved x-bridge.* headers are stripped to prevent an
//     external producer from injecting bridge metadata (idempotency,
//     routing, correlation).
//   - EGRESS: internal-only reserved headers (route-id, route-override,
//     source-id, content-type) are stripped as dispatch bookkeeping that
//     must not leak, but bridge-to-bridge propagated headers
//     (correlation-id, causation-id, idempotency-key, tenant-id,
//     forwarded-from/hop, traceparent/tracestate) are preserved on the
//     wire so a peer bridge on the next hop can correlate, deduplicate,
//     continue a trace, and break forwarding loops.
//
// # Topology Arguments
//
// Subscription and publisher declarations accept argument tables
// (SubscriptionParams.QueueArguments / ExchangeArguments /
// BindingArguments and PublisherParams.ExchangeArguments). These flow
// verbatim into the AMQP queue/exchange/bind declarations, enabling
// quorum queues (x-queue-type), dead-letter exchanges, message TTL,
// length limits, and headers-exchange bindings. Numeric values should be
// integers; the broker rejects a float where it expects an integer.
//
// # Failover Boundary
//
// A Session connects to a single broker_url. High availability is
// delegated to the infrastructure in front of that URL — a load balancer,
// a DNS name that resolves to the live node, or a RabbitMQ cluster behind
// one virtual address. The adapter does NOT rotate through a list of
// broker URLs: on disconnect it reconnects (with backoff) to the same
// configured URL. Multi-endpoint client-side failover is intentionally
// out of scope; deploy a clustered/HA endpoint instead.
//
// # Error Mapping
//
// amqp091.Error codes are classified into shared.BridgeError categories:
//
//   - 320 (connection-forced)         -> ErrConnectionLost (transient)
//   - 403 (access-refused)            -> ErrNotAuthorized  (permanent)
//   - 404 (not-found)                 -> ErrNotFound       (permanent)
//   - 405 (not-allowed), 530          -> ErrForbidden      (permanent)
//   - 406, 540 (not-implemented)      -> ErrNotSupported   (permanent)
//   - 501, 502, 503, 505 (protocol)   -> ErrProtocolError  (permanent)
//   - 504 (channel-error), 541        -> ErrUnavailable    (transient)
//
// A managed receiver fails the component on a permanent error (it would
// recur identically on every reconnect) and applies a bounded backoff
// between retries of transient failures, so a persistently failing
// consumer neither hot-loops nor silently retries forever.
package amqp091

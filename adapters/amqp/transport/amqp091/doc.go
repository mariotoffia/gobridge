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
// AMQP 0-9-1 has no client-side delayed redelivery: Retry(after > 0)
// nacks with IMMEDIATE requeue and surfaces the unhonored delay via the
// AMQP091DelayedRetryUnhonored metric (every occurrence) and a Warn log
// (once per consumer channel). Guard poison messages broker-side with
// x-delivery-limit (quorum queues) or a dead-letter exchange; without
// one, a message that always fails hot-loops on a classic queue.
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
// Publishes are PERSISTENT by default (AMQP delivery-mode 2): a confirmed
// transient message exists only in broker memory and dies with a broker
// restart even on a durable classic queue — after the bridge has already
// acked the source. sender.delivery_mode ("persistent"|"transient")
// selects the default; a per-message "amqp091.delivery-mode" envelope
// header (uint8, int, float, "1"/"2"/"transient"/"persistent") overrides
// it. Quorum queues persist messages regardless of this knob; it matters
// for durable classic queues.
//
// SendBatch pipelines non-mandatory batches: every message is published
// with a deferred confirmation and the confirms are awaited afterwards,
// so batch throughput is not bounded to one confirm round-trip per
// message. Per-message error attribution is preserved. Mandatory batches
// stay sequential (one in flight): a basic.return carries no delivery
// tag, so return-to-message attribution relies on ordering.
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
//
// Exception: two permanent-classified codes are, in a reconnect window,
// transient broker races and are retried with a bounded budget (see
// reconnectRaceRetryBudget) before failing the component: 404 (a consume
// racing the session's topology reconcile after a broker restart) and
// 403 on an EXCLUSIVE consumer (the broker holds the stale exclusive
// consumer for ~2x heartbeat after a partition). Each retry emits the
// AMQP091ReconnectRaceRetried metric and a Warn.
//
// # Session Lifecycle
//
// Start dials and, when a SessionPlan is installed, reconciles before
// returning: a nil Start means connected AND declared topology in place.
// A failed initial reconcile fails Start (the queue would be unbound and
// messages silently unroutable). After a reconnect, the session reports
// Connected and emits SessionConnected only once reconcile has succeeded;
// a failed reconcile drops the fresh connection and retries the whole
// attempt with backoff. Close is safe to race with Start or a reconnect:
// a connection dialed after Close began is closed, never installed.
package amqp091

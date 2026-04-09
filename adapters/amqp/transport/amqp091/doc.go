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
// # Header Mapping
//
// AMQP 0-9-1 system properties (MessageId, CorrelationId, ContentType,
// ContentEncoding, ReplyTo, Type, AppId, DeliveryMode, Priority,
// Expiration, Timestamp) are mapped to envelope headers with the
// "amqp091." prefix. User-defined headers from the AMQP Headers table
// are mapped directly. Reserved x-bridge.* headers are stripped at
// ingress to prevent header injection.
//
// # Error Mapping
//
// amqp091.Error codes are classified into domain.BridgeError categories:
//
//   - 320 (connection-forced)         -> ErrConnectionLost (transient)
//   - 403 (access-refused)            -> ErrNotAuthorized  (permanent)
//   - 404 (not-found)                 -> ErrNotFound       (permanent)
//   - 405 (not-allowed), 530          -> ErrForbidden      (permanent)
//   - 406, 540 (not-implemented)      -> ErrNotSupported   (permanent)
//   - 501, 502, 503, 505 (protocol)   -> ErrProtocolError  (permanent)
//   - 504 (channel-error), 541        -> ErrUnavailable    (transient)
package amqp091

// Package amqp10 implements ports.Session, ports.Receiver, ports.Sender,
// and ports.BatchSender for AMQP 1.0 (OASIS standard) brokers such as
// Apache ActiveMQ Artemis, Solace PubSub+, and Apache Qpid.
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
//   - Retry(after>0): ModifyMessage   (DeliveryFailed=true, signal broker retry)
//   - Extend:         ErrNotSupported (AMQP 1.0 uses credit-based flow control,
//     not visibility timeouts)
//
// # Header Mapping
//
// Standard AMQP 1.0 message properties (message-id, correlation-id,
// content-type, subject, to, reply-to, etc.) are mapped to envelope
// headers with the "amqp10." prefix. Application properties are mapped
// directly as envelope headers. Reserved bridge headers (x-bridge.*)
// are stripped from incoming messages at ingress.
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
package amqp10

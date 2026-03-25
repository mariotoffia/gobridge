// Package servicebus implements ports.Receiver, ports.Sender, and
// ports.BatchSender for Azure Service Bus.
//
// # Architecture
//
// Azure Service Bus is treated as a sessionless transport (like SQS).
// The Azure SDK manages AMQP connections internally. ASB "sessions"
// (message grouping) map to ReceiverConfig.SessionID, not ports.Session.
//
// # Receiver
//
// The Receiver calls ReceiveMessages on a queue or topic subscription
// and emits each message as a ports.Delivery whose operations map to
// Service Bus receipt-handle actions:
//
//   - Ack:    CompleteMessage
//   - Retry:  AbandonMessage
//   - Extend: RenewMessageLock
//
// When AutoExtend is enabled (the default), a background goroutine
// periodically renews the lock at 50 % of the configured lock duration,
// preventing Service Bus from making the message visible to other
// consumers while the bridge pipeline is still processing it.
//
// # Sender
//
// The Sender submits envelopes to a Service Bus queue or topic, mapping
// Envelope.Headers to ApplicationProperties. It implements
// ports.BatchSender for efficient multi-message sends. ASB batches are
// size-limited (not count-limited like SQS); the sender handles
// ErrMessageTooLarge by flushing the current batch and retrying
// individually.
//
// # Header Mapping
//
// Standard bridge headers (x-bridge.*) are mapped to/from ASB
// ApplicationProperties. Well-known ASB system properties (MessageID,
// CorrelationID, SessionID, Subject, ContentType, etc.) are mapped to
// headers with the asb. prefix.
//
// # Error Mapping
//
// Azure SDK errors are classified into domain.BridgeError categories:
//
//   - CodeTimeout        -> ErrTimeout       (transient)
//   - CodeConnectionLost -> ErrConnectionLost (transient)
//   - CodeLockLost       -> ErrUnavailable    (transient)
//   - CodeUnauthorizedAccess -> ErrNotAuthorized (permanent)
//   - ErrMessageTooLarge -> ErrPayloadTooLarge (rejected)
//
// Azure Service Bus is stateless and does not require a Session.
package servicebus

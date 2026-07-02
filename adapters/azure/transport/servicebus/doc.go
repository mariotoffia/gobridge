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
//   - Retry:  AbandonMessage, or a scheduled copy + Complete (see below)
//   - Extend: RenewMessageLock
//
// When AutoExtend is enabled (the default), a background goroutine
// periodically renews the lock at 50 % of the configured lock duration,
// preventing Service Bus from making the message visible to other
// consumers while the bridge pipeline is still processing it. If lock
// renewal fails autoExtendMaxFailures times in a row the goroutine
// cancels the delivery's processing context — the same context handed
// to the emit callback — so the in-flight pipeline aborts rather than
// continuing to work a message whose lock has lapsed. Likewise, when the
// emit callback returns an error the poll loop cancels that context, so
// auto-extend stops and the broker lock is allowed to expire for
// redelivery; the message is never left silently held after Run returns.
//
// # Receive modes
//
// PeekLock (the default) locks each message and requires explicit
// settlement (Ack/Retry). ReceiveAndDelete removes the message from the
// broker at receive time, so there is no lock to settle: auto-extend is
// disabled, Ack and Extend are no-ops, and Retry reports
// shared.ErrNotSupported. The runtime then DLQ-routes the message rather
// than looping on a retry that can never take effect. ReceiveAndDelete is
// therefore at-most-once: a processing failure with no DLQ configured
// drops the message — choose PeekLock when no-loss is required.
//
// # Delayed retry (Retry with a non-zero delay)
//
// A queue receiver schedules a delayed copy of the message via the
// Service Bus scheduler (ScheduleMessages) and completes the original,
// preserving the requested delay. A topic-subscription receiver does NOT
// schedule: a scheduled message addresses the entity by name, and a
// subscription's entity name is the TOPIC, so a scheduled copy would fan
// out to every sibling subscription and duplicate the message. For
// subscriptions the delayed retry falls back to AbandonMessage
// (immediate same-subscription redelivery, no fan-out); the requested
// delay is not honoured.
//
// # Dead-lettering (boundary)
//
// This adapter does not call native DeadLetterMessage. Permanent
// processing failures are routed to the GoBridge DLQ by the runtime and
// the source message is then settled with CompleteMessage — the intended,
// primary dead-letter path, where a message is dead-lettered exactly once.
//
// There is a SECOND dead-letter sink outside the adapter's control: the
// broker's MaxDeliveryCount. Paths that release a message with
// AbandonMessage (malformed deliveries, and delayed retry on a topic
// subscription) increment the broker delivery count; once it exceeds
// MaxDeliveryCount the broker moves the message to its native DLQ. So the
// "exactly once, one place" guarantee holds only on the runtime-DLQ path —
// abandon-based redelivery can additionally land in the broker DLQ.
// Operators should size MaxDeliveryCount accordingly and monitor the
// native DLQ, which remains reachable for *reading* via
// ReceiverConfig.SubQueue ("deadletter") even though this adapter never
// writes to it explicitly.
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
// # Receiver tuning
//
// ReceiverConfig.MaxWaitTime bounds a single ReceiveMessages long-poll.
// The Azure SDK exposes no "max wait for the first message" option, so it
// is applied as a per-receive context deadline; an elapsed deadline with
// no messages is a normal idle poll, not a transport error.
// ReceiverConfig.Prefetch is accepted for forward compatibility but is
// currently a no-op: azservicebus v1.10.0 exposes no public prefetch
// knob (prefetch is managed internally by the SDK's AMQP credit
// machinery). A non-zero value is warned about at receiver construction.
//
// # Header Mapping
//
// Standard bridge headers (x-bridge.*) are mapped to/from ASB
// ApplicationProperties. Well-known ASB system properties (MessageID,
// CorrelationID, SessionID, Subject, ContentType, etc.) are mapped to
// headers with the asb. prefix.
//
// Reserved-header policy is applied at the ACL boundary. On ingress every
// reserved x-bridge.* ApplicationProperty is dropped (IsReservedHeader)
// so an external publisher cannot spoof bridge bookkeeping. On egress only
// the INTERNAL-ONLY reserved headers are stripped (IsInternalOnlyHeader);
// bridge-to-bridge propagated headers (correlation, idempotency, tracing,
// tenant, forwarded-*) and application headers pass through so a peer
// bridge can correlate, deduplicate and continue a trace.
//
// # Error Mapping
//
// Azure SDK errors are classified into shared.BridgeError categories:
//
//   - CodeTimeout        -> ErrTimeout       (transient)
//   - CodeConnectionLost -> ErrConnectionLost (transient)
//   - CodeLockLost       -> ErrUnavailable    (transient)
//   - CodeUnauthorizedAccess -> ErrNotAuthorized (permanent)
//   - ErrMessageTooLarge -> ErrPayloadTooLarge (rejected)
//
// Azure Service Bus is stateless and does not require a Session.
package servicebus

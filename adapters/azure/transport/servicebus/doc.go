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
// consumers while the bridge pipeline is still processing it. Renewal
// starts for EVERY message of a received batch immediately after the
// batch arrives — before the (possibly blocking) emit loop — so the
// locks of batched-but-not-yet-emitted messages cannot lapse under
// backpressure. If lock renewal fails autoExtendMaxFailures times in a
// row the goroutine cancels the delivery's processing context — the same
// context handed to the emit callback — so the in-flight pipeline aborts
// rather than continuing to work a message whose lock has lapsed.
// Likewise, when the emit callback returns an error the poll loop
// cancels that context (and the contexts of every not-yet-emitted
// delivery of the batch), so auto-extend stops and the broker lock is
// allowed to expire for redelivery; the message is never left silently
// held after Run returns.
//
// Total renewal per delivery is capped by
// ReceiverConfig.MaxLockRenewalDuration (default 5m): when the cap is
// reached, renewal stops, the processing context is cancelled, and
// MetricASBLockRenewalCapExceeded is incremented — a hung pipeline can
// no longer keep a message locked (invisible, never redelivered, never
// dead-lettered) forever.
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
// drops the message — choose PeekLock when no-loss is required. Because
// the broker settles at receive time, MaxMessages is forced to 1 in this
// mode (a larger batch would lose every received-but-not-yet-emitted
// message on shutdown); a clamped configuration is warned about at
// receiver construction.
//
// ReceiveMode and SubQueue are validated strictly (case-insensitive
// against the known values); an unknown spelling such as "dead-letter"
// is a configuration error instead of silently selecting the default —
// which for sub_queue would mean consuming the MAIN queue during a DLQ
// redrive.
//
// # Sessions
//
// ReceiverConfig.SessionID locks the receiver to one ASB session.
// Session accept retries with backoff (up to sessionAcceptMaxAttempts):
// com.microsoft:session-cannot-be-locked is expected during rolling
// deploys while the outgoing pod still holds the session lock, and must
// not crash-loop the bridge. SessionID cannot be combined with SubQueue
// (the SDK's SessionReceiverOptions has no sub-queue selector); the
// combination is rejected at validation. A NON-session receiver on a
// session-enabled entity fails fast with shared.ErrNotSupported instead
// of warn-looping forever; accept-next-session polling is not
// implemented. In session mode all in-flight deliveries share ONE
// session lock, so a single session-renewer goroutine replaces the
// per-delivery auto-extend goroutines; MaxLockRenewalDuration does not
// apply to the session renewer (the session lock must be held for the
// life of the poll loop).
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
// Design note: the subscription abandon-instead-of-schedule fallback
// (dropping the requested delay to avoid topic fan-out) is a deliberate
// trade-off, not a limitation worked around here — honouring the delay on
// a subscription would require a per-subscription scheduling primitive
// that Service Bus does not expose.
//
// The scheduled copy (buildRetryMessage) carries three safety measures:
//
//   - Attempt accounting: scheduling a fresh copy resets the broker
//     DeliveryCount to 1, so the copy carries the accumulated receive
//     count in the reserved x-bridge.retry-attempt application
//     property. Ingress adds it back onto the broker DeliveryCount when
//     stamping the asb.delivery-count header, so the runtime's
//     MaxReplayAttempts gate keeps firing across bridge-scheduled
//     retries and poison messages reach the DLQ instead of ping-ponging
//     forever.
//   - Duplicate detection: the copy's MessageID is salted with the
//     attempt number ("<original>-r<n>") so a dedup-enabled queue
//     (RequiresDuplicateDetection) never silently discards the
//     scheduled retry inside its dedup window. The FIRST delivery's
//     MessageID is preserved in x-bridge.original-message-id and
//     restored as the envelope ID on ingress, so end-to-end
//     idempotency still sees one logical message.
//   - TTL: the copy's TimeToLive is the REMAINING time until the
//     original absolute expiry (ExpiresAt), not a restart of the full
//     TTL per retry.
//
// # Dead-lettering (boundary)
//
// This adapter does not call native DeadLetterMessage. Permanent
// processing failures are routed to the GoBridge DLQ by the runtime and
// the source message is then settled with CompleteMessage — the intended,
// primary dead-letter sink.
//
// This is NOT exactly-once. End-to-end the bridge is at-least-once with
// idempotency-based deduplication: the DLQ write and the source
// CompleteMessage are two separate steps, so a crash or a settlement
// failure between them leaves the PeekLock to expire, the broker
// redelivers, and the runtime can route the same message to the DLQ
// again. Consumers (and the bridge's own dedup) must key on the
// idempotency header rather than assume a single dead-letter event.
//
// There is also a SECOND dead-letter sink outside the adapter's control:
// the broker's MaxDeliveryCount. Paths that release a message with
// AbandonMessage — an ordinary transient Retry, a malformed delivery, or a
// delayed retry on a topic subscription (see above) — increment the broker
// delivery count; once it exceeds MaxDeliveryCount the broker moves the
// message to its own native DLQ, which is DISTINCT from the GoBridge DLQ.
// A single logical failure can therefore surface in either sink (and,
// across redeliveries, occasionally both). Operators should size
// MaxDeliveryCount accordingly and monitor the native DLQ, which remains
// reachable for *reading* via ReceiverConfig.SubQueue ("deadletter") even
// though this adapter never writes to it explicitly.
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
// no messages is a normal idle poll, not a transport error. Config
// validation floors max_wait_time at 1s (a sub-second value turns the
// long-poll into a hot loop). There is no prefetch knob: azservicebus
// v1.10.0 manages AMQP credit internally and exposes no public prefetch
// option.
//
// # Credential rotation
//
// ApplyCredentials on both Receiver and Sender builds a complete
// replacement stack from the rotated material and atomically swaps it
// in while traffic is flowing; the poll loop / send paths snapshot the
// live client under a lock and are never exposed to a nil or
// half-initialised client. In-flight operations finish against the old
// link; unsettled locks then lapse and the broker redelivers
// (at-least-once). For session receivers the old link is closed first
// (the new accept cannot obtain the session lock while the old holder
// lives), so rotation has a short receive-error gap bridged by poll
// backoff.
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

// Package sqs implements ports.Receiver, ports.Sender, and ports.BatchSender
// for Amazon SQS.
//
// # Receiver
//
// The Receiver long-polls an SQS queue and emits each message as a
// ports.Delivery whose operations map to SQS receipt-handle actions:
//
//   - Ack:    DeleteMessage
//   - Retry:  ChangeMessageVisibility (with configurable delay)
//   - Extend: ChangeMessageVisibility (to a future absolute time)
//
// When AutoExtend is enabled (the default), a background goroutine
// periodically extends visibility at 50 % of the configured timeout,
// preventing SQS from making the message visible to other consumers
// while the bridge pipeline is still processing it.
//
// # Sender
//
// The Sender submits envelopes to an SQS queue, mapping Envelope.Headers
// to SQS message attributes and supporting both standard and FIFO queues.
// It implements ports.BatchSender for efficient multi-message sends.
//
// # Header Mapping
//
// Bridge headers are mapped to/from SQS message attributes asymmetrically,
// following the central egress header policy:
//
//   - INGRESS (attributesToHeaders): message attributes whose names carry
//     the reserved x-bridge.* prefix are stripped, so an external producer
//     cannot spoof bridge metadata (idempotency, routing, correlation).
//   - EGRESS (headersToAttributes): INTERNAL-ONLY reserved headers
//     (route-id, route-override, source-id, content-type) are stripped —
//     they are dispatch bookkeeping that must not leak. BRIDGE-TO-BRIDGE
//     propagated headers (correlation-id, causation-id, idempotency-key,
//     tenant-id, forwarded-from/hop, traceparent/tracestate) are preserved
//     as SQS message attributes so a peer bridge on the next hop can
//     correlate, deduplicate and continue a trace. Under the 10-attribute
//     SQS cap, however, only the ESSENTIAL propagation headers
//     (idempotency-key, traceparent, tracestate) outrank application
//     headers; the remaining bridge-to-bridge headers are sacrificed
//     FIRST, because a peer bridge's ingress strips reserved x-bridge.*
//     attributes anyway — dropping real application data to keep them
//     would lose data for headers the next hop discards. Application
//     headers are otherwise preserved as-is.
//   - messaging.HeaderOrderingKey maps to the native MessageGroupId and
//     messaging.HeaderDeduplicationID maps to the native
//     MessageDeduplicationId for FIFO queues; neither is also emitted as a
//     message attribute.
//
// Consequence of the asymmetry: egress now PRESERVES bridge-to-bridge
// headers on the wire, but this adapter's DEFAULT ingress still strips
// every reserved x-bridge.* attribute, because SQS cannot distinguish a
// trusted peer bridge from an untrusted external producer. A peer bridge
// therefore re-stamps its own correlation/idempotency on receive. An
// opt-in "trusted peer" ingress mode that honours bridge-to-bridge
// attributes from a verified source requires shared ports configuration
// and is intentionally out of scope for this adapter (see Phase 1b).
//
// Cross-hop identity lift (the exception to the wholesale strip above):
// while the header MAP still drops every x-bridge.* attribute, SQS ingress
// (convertMessage) additionally LIFTS the bridge-to-bridge IDENTITY a peer
// bridge's egress propagates — the idempotency key from the
// x-bridge.idempotency-key message attribute, and the dedup/ordering keys
// from the native FIFO fields MessageDeduplicationId / MessageGroupId —
// into the envelope's first-class IdempotencyKey / DeduplicationID /
// OrderingKey fields. messaging.NewEnvelope re-stamps those into their
// reserved headers AFTER the anti-spoof strip, so a peer bridge on the next
// hop can deduplicate and preserve message ordering across an SQS hop even
// though the raw x-bridge.* attributes never survive the header map. The
// dedup/ordering keys are lifted ONLY from the native FIFO fields, never
// from x-bridge.dedup-id / x-bridge.ordering-key attributes (egress never
// emits those as attributes). This mirrors the amqp10 adapter and is a
// deliberately narrow identity lift, distinct from the broader "trusted
// peer" ingress mode (correlation, causation, tenant, trace) that remains
// out of scope.
//
// # Attribute Limits
//
// SQS rejects a send that carries more than 10 message attributes, an
// invalid attribute name, or an oversized message. headersToAttributes
// enforces these deterministically rather than letting one bad envelope
// fail every send: attribute names that SQS would reject (charset,
// length, AWS./Amazon. reserved prefix, leading/trailing/consecutive
// periods) are dropped; the remaining eligible headers are ranked
// (essential propagation headers, then application headers, then other
// bridge-to-bridge headers; by name within a rank) and the
// highest-ranked are kept up to the 10-attribute cap (one slot is
// reserved for the Subject attribute when present); the cumulative size
// is bounded by the SQS message maximum. Headers dropped by the
// count/size caps are reported via the SQSDroppedAttributes counter and
// a debug log so the loss is observable.
//
// # FIFO Ordering
//
// A FIFO source (queue URL or name ending in ".fifo") forces
// MaxMessages=1 on ReceiveMessage. The runtime dispatches deliveries
// concurrently, so returning several messages of one MessageGroupId in a
// single receive could reorder them; SQS keeps a group locked to its
// in-flight message until that message is deleted, so receiving one at a
// time preserves per-group order without serialising in the shared route
// runner. FIFO dedup, when the producer supplies no explicit
// MessageDeduplicationId, is derived from a hash of the payload, subject
// and id (or creation time when id is empty) — not the subject alone.
//
// A FIFO TARGET queue (SenderConfig.QueueURL/QueueName ending in
// ".fifo") requires a message group: NewSender fails fast unless
// MessageGroupID (a default group) is configured or FIFO is true
// (per-envelope group via the x-bridge.ordering-key header). With FIFO
// enabled but no group resolvable for a given envelope, Send/SendBatch
// reject that envelope with shared.ErrInvalidPayload before any SDK
// call — a deterministic configuration fault is never retried as
// transient.
//
// # Subject and Address
//
// The logical Envelope.Subject is mapped to the "Subject" SQS message
// attribute on egress and read back from that attribute on ingress.
// The receiver does NOT fall back to the queue name or queue URL when
// the "Subject" attribute is absent — Envelope.Subject is left empty
// in that case. When SNSUnwrap is enabled and the body is a JSON
// document with Type == "Notification" and a non-empty TopicArn, the
// inner SNS Subject (if present) is used instead; when the SNS
// notification has no Subject field the TopicArn is preserved in
// headers["sns.topic_arn"] but is NOT promoted into Envelope.Subject.
// Bodies without the Type field are passed through verbatim — a bare
// TopicArn key is not enough to forge sns.* metadata. The unwrap still
// trusts the queue policy to restrict sqs:SendMessage to the intended
// SNS topic; it is a format check, not an authenticity check.
//
// SQS senders are bound to a single queue. ports.OutboundMessage.Address
// is validated against that queue: empty means "use the configured
// queue"; a value that refers to the bound queue succeeds — the resolved
// queue URL, the configured queue name, or the queue name embedded as the
// URL's last path segment (the form scenario configs use, e.g.
// `address: orders`); anything else is rejected with
// shared.ErrInvalidTopic without contacting the SDK. Per-message dynamic
// addressing for SQS is out of scope.
//
// # SQS Native DLQ
//
// SQS native DLQ (maxReceiveCount on the source queue) should be set to
// at least bridge max retries + 3 to act as a safety net for
// infrastructure failures that prevent the bridge from processing the
// message at all. The bridge's own DLQ handles application-level
// permanent failures, expired messages, and policy rejections.
//
// # Capabilities
//
// The factory declares CapVisibilityExtension, CapSourceRedelivery and
// CapDelayedSend. CapSharedConsumer is intentionally NOT declared: SQS is
// a competing-consumer work queue (each message goes to one consumer and
// scaling out across pollers is the intended mode), whereas
// CapSharedConsumer denotes a broadcast source that the runtime validator
// forces into single-active fencing. Declaring it would reject every
// unfenced SQS direct_hold route.
//
// # Batch Sends
//
// The Sender implements ports.BatchSender and SenderConfig.BatchSize
// chunks SendMessageBatch calls. The bridge's normal per-delivery dispatch
// path does not currently call SendBatch, so batch_size only affects
// callers that invoke SendBatch directly; it does not reduce per-message
// API calls on the standard route. Batch-aware runtime dispatch is a
// shared-runtime concern outside this adapter.
//
// # Concurrency and Credential Rotation
//
// The underlying SQS client is held in an atomic.Pointer snapshot. Hot
// send/receive paths read it lock-free; ApplyCredentials and lazy
// initialisation swap a rotated client under an internal mutex. In-flight
// calls keep using the client they loaded; subsequent calls pick up the
// rotated credentials. There is no client read/write data race.
//
// SQS is stateless and does not require a Session.
package sqs

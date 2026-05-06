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
// Standard bridge headers (x-bridge.*) are mapped to/from SQS message
// attributes. At ingress, headers with the reserved x-bridge.* prefix are
// stripped to prevent injection from external sources. At egress,
// messaging.HeaderOrderingKey maps to MessageGroupId and
// messaging.HeaderDeduplicationID maps to MessageDeduplicationId for FIFO
// queues.
//
// # SQS Native DLQ
//
// SQS native DLQ (maxReceiveCount on the source queue) should be set to
// at least bridge max retries + 3 to act as a safety net for
// infrastructure failures that prevent the bridge from processing the
// message at all. The bridge's own DLQ handles application-level
// permanent failures, expired messages, and policy rejections.
//
// SQS is stateless and does not require a Session.
package sqs

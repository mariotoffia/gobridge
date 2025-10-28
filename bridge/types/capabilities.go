package types

type CapabilityType string

const (
	// CapabilityReceiveExactOnce indicates that the connection/topic supports exact-once delivery semantics and
	// thus, will ack message when successfully processed by `Subscriber`.
	//
	// In combination with a `Subscriber` that supports idempotent processing (like _AWS_ SQS FIFO queue), this will ensure that
	// each message is processed exactly once.
	CapabilityReceiveExactOnce CapabilityType = "ReceiveOnce"
	// CapabilityReceiveAtLeastOnce indicates that the connection/topic supports at-least-once delivery semantics and
	// thus, will re-deliver messages that were not acked by the `Subscriber`.
	//
	// This is the default behavior for most connections.
	CapabilityReceiveAtLeastOnce CapabilityType = "ReceiveAtLeastOnce"
	// CapabilityReceiveAtMostOnce indicates that the connection/topic supports at-most-once delivery semantics and
	// thus, will not re-deliver messages even if they were not acked by the `Subscriber`.
	//
	// This means that some messages may be lost if the `Subscriber` fails to process them.
	CapabilityReceiveAtMostOnce CapabilityType = "ReceiveAtMostOnce"
	// CapabilityPublishExactOnce indicates that the connection/topic supports exact-once delivery semantics for publishing messages.
	//
	// This means that the connection will ensure that each published message is delivered exactly once to the remote server.
	CapabilityPublishExactOnce CapabilityType = "PublishOnce"
	// CapabilityPublishAtLeastOnce indicates that the connection/topic supports at-least-once delivery semantics for publishing messages.
	//
	// This means that the connection may re-attempt to deliver the published message until it receives an acknowledgment from the remote server.
	//
	// This is the default behavior for most connections.
	CapabilityPublishAtLeastOnce CapabilityType = "PublishAtLeastOnce"
	// CapabilityPublishAtMostOnce indicates that the connection/topic supports at-most-once delivery semantics for publishing messages.
	//
	// This means that the connection will attempt to deliver the published message only once, without any retries.
	//
	// Some messages may be lost if the initial delivery attempt fails.
	CapabilityPublishAtMostOnce CapabilityType = "PublishAtMostOnce"
	// CapabilityPublishInMemoryRetryable indicates that the connection/topic supports in memory retryable publishes.
	//
	// This means that if a publish fails due to a temporary error, the connection will retry the publish
	// using an in-memory queue until it succeeds or a permanent error occurs. It will, however, not persist
	// messages to disk for retrying later.
	CapabilityPublishInMemoryRetryable CapabilityType = "PublishInMemoryRetryable"
)

// Capability is exposed by the `Connection` to indicate supported features or settings.
type Capability struct {
	Type  string `json:"type"`
	Value any    `json:"value,omitempty"`
}

// Capabilities is a slice of Capability interfaces.
type Capabilities []Capability

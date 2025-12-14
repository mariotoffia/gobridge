package types

import (
	"context"
	"io"
)

// ============================================================================
// Source Interface
// ============================================================================

// Source represents a message source that receives messages from an external system.
// Examples: SQS queue, MQTT subscription, Azure Service Bus subscription, Kafka consumer.
//
// A Source is typically the ingress point of a Pipeline.
//
// # Implementor Contract
//
// Source implementations MUST follow these rules:
//
// ## Capability Reporting
//
// Sources MUST accurately report their capabilities via Capabilities():
//
//   - CapabilityRedelivery: Set if Nack() can return messages for redelivery
//   - CapabilityExtendTimeout: Set if Extend() is functional
//   - CapabilityOrdering: Set if messages are delivered in order
//   - CapabilityReceiveAtLeastOnce/ExactOnce/AtMostOnce: Delivery semantics
//
// ## SourceMessage Callbacks
//
// The Ack/Nack/Extend functions on SourceMessage have specific contracts:
//
//   - Ack(): MUST be idempotent (safe to call multiple times)
//   - Nack(): SHOULD redeliver if capable (report CapabilityRedelivery)
//   - Extend(): Return nil if not supported (don't report CapabilityExtendTimeout)
//
// ## Context Cancellation
//
// Sources MUST handle context cancellation gracefully for shutdown.
// When context is cancelled:
//  1. Stop receiving new messages
//  2. Close the Messages() channel
//  3. Allow in-flight messages to complete Ack/Nack
type Source interface {
	io.Closer

	// GetID returns the unique identifier of the source.
	GetID() string

	// GetTransportType returns the transport type (e.g., "MQTT", "SQS").
	GetTransportType() TransportType

	// Start begins receiving messages. The source will emit messages until
	// the context is cancelled or Close is called.
	Start(ctx context.Context) error

	// Messages returns a channel that receives messages from the source.
	// This channel is closed when the source is stopped.
	Messages() <-chan *SourceMessage

	// Capabilities returns the capabilities of this source.
	// Implementations MUST report accurate capabilities.
	Capabilities() Capabilities
}

// ============================================================================
// SourceMessage
// ============================================================================

// SourceMessage wraps a Message with acknowledgment capabilities.
// This allows the source technology to track message processing.
//
// # Callback Contract
//
// ## Ack()
//
// Signals successful processing. Source implementations SHOULD:
//   - Delete/commit the message from the source system
//   - Be idempotent (multiple calls are safe and have no additional effect)
//   - Return error only if the acknowledgment itself failed
//
// The pipeline calls Ack() when:
//   - Target.Send() returns nil (success)
//   - Message is enqueued to RetryManager (retry takes ownership)
//   - Message is archived to DeadLetterQueue (DLQ takes ownership)
//
// ## Nack(reason error)
//
// Signals processing failure. Source implementations SHOULD:
//   - Return message to source system for redelivery (if capable)
//   - Report CapabilityRedelivery if redelivery is supported
//   - Log the failure reason if redelivery is not supported
//
// The reason parameter describes why processing failed and can be
// used for logging or metrics.
//
// Note: The pipeline typically does NOT call Nack() directly. Instead,
// it uses RetryManager or DLQ to take ownership, then calls Ack().
// Nack() is available for custom error handling scenarios.
//
// ## Extend(ctx context.Context)
//
// Extends the processing deadline for this message.
//   - Sources that support visibility timeout SHOULD implement this
//   - Report CapabilityExtendTimeout if functional
//   - Return nil if not supported (no error, just no-op)
//
// Use this for long-running message processing to prevent timeout.
type SourceMessage struct {
	// Message is the actual message content.
	Message Message

	// Ack acknowledges successful processing.
	//
	// Contract:
	//   - MUST be idempotent (safe to call multiple times)
	//   - SHOULD delete/commit message from source system
	//   - Return error only if acknowledgment operation failed
	Ack func() error

	// Nack indicates processing failure.
	//
	// Contract:
	//   - SHOULD redeliver message if source supports it
	//   - Report CapabilityRedelivery if redelivery works
	//   - The reason describes why processing failed
	Nack func(reason error) error

	// Extend extends the processing deadline for this message.
	//
	// Contract:
	//   - Return nil if not supported (no-op)
	//   - Report CapabilityExtendTimeout if functional
	//   - Use for long-running processing to prevent timeout
	Extend func(ctx context.Context) error
}

// ============================================================================
// Source Configuration
// ============================================================================

// SourceConfig defines configuration for creating a Source.
type SourceConfig interface {
	Config
	ResourceBasedLookupConfig

	// GetQoS returns the desired QoS level when receiving messages.
	// Returns nil if the transport doesn't support QoS levels.
	GetQoS() *QosLevel

	// GetPrefetch returns the number of messages to prefetch (if supported).
	GetPrefetch() int
}

// ============================================================================
// Source Factory and Registry
// ============================================================================

// SourceFactory creates Source instances from configuration.
type SourceFactory interface {
	// CreateSource creates a new Source from the given configuration.
	CreateSource(ctx context.Context, config SourceConfig) (Source, error)

	// SupportedTransports returns the transport types this factory can create.
	SupportedTransports() []TransportType
}

// SourceRegistry manages Source factories.
type SourceRegistry interface {
	// RegisterFactory registers a factory for creating sources.
	RegisterFactory(factory SourceFactory)

	// CreateSource creates a source using the appropriate registered factory.
	CreateSource(ctx context.Context, config SourceConfig) (Source, error)
}

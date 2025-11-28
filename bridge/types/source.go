package types

import (
	"context"
	"io"
)

// Source represents a message source that receives messages from an external system.
// Examples: SQS queue, MQTT subscription, Azure Service Bus subscription, Kafka consumer.
//
// A Source is typically the ingress point of a Pipeline.
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
	Capabilities() Capabilities
}

// SourceMessage wraps a Message with acknowledgment capabilities.
// This allows the source technology to track message processing.
type SourceMessage struct {
	// Message is the actual message content.
	Message Message
	// Ack acknowledges successful processing. For technologies like SQS,
	// this deletes the message from the queue.
	Ack func() error
	// Nack indicates processing failure. For technologies that support it,
	// this may trigger redelivery or move to a dead-letter queue.
	// The error parameter describes why processing failed.
	Nack func(reason error) error
	// Extend extends the visibility timeout for this message.
	// Not all technologies support this (returns nil if unsupported).
	Extend func(ctx context.Context) error
}

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


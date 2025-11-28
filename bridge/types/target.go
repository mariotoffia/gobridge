package types

import (
	"context"
	"io"
	"time"
)

// Target represents a message destination that sends messages to an external system.
// Examples: MQTT publish, SQS send, Azure Service Bus send, Kafka producer.
//
// A Target is typically the egress point of a Pipeline.
type Target interface {
	io.Closer
	// GetID returns the unique identifier of the target.
	GetID() string
	// GetTransportType returns the transport type (e.g., "MQTT", "SQS").
	GetTransportType() TransportType
	// Send sends a message to the target destination.
	// The topic/queue is typically embedded in the message or configured in the target.
	Send(ctx context.Context, msg Message) error
	// SendBatch sends multiple messages in a single operation if supported.
	// Returns the number of successfully sent messages and any error.
	// If batching is not supported, it sends messages sequentially.
	SendBatch(ctx context.Context, msgs []Message) (sent int, err error)
	// Capabilities returns the capabilities of this target.
	Capabilities() Capabilities
}

// TargetConfig defines configuration for creating a Target.
type TargetConfig interface {
	Config
	ResourceBasedLookupConfig
	// GetDefaultQoS returns the default QoS level for sending messages.
	// Individual messages may override this.
	GetDefaultQoS() *QosLevel
	// GetBatchSize returns the preferred batch size for SendBatch.
	// Returns 0 if batching is not configured.
	GetBatchSize() int
	// GetTimeout returns the timeout for send operations.
	GetTimeout() *time.Duration
}

// TargetFactory creates Target instances from configuration.
type TargetFactory interface {
	// CreateTarget creates a new Target from the given configuration.
	CreateTarget(ctx context.Context, config TargetConfig) (Target, error)
	// SupportedTransports returns the transport types this factory can create.
	SupportedTransports() []TransportType
}

// TargetRegistry manages Target factories.
type TargetRegistry interface {
	// RegisterFactory registers a factory for creating targets.
	RegisterFactory(factory TargetFactory)
	// CreateTarget creates a target using the appropriate registered factory.
	CreateTarget(ctx context.Context, config TargetConfig) (Target, error)
}

// TargetResult represents the result of sending a message to a target.
// Used for detailed tracking in pipelines.
type TargetResult struct {
	// MessageID is the ID assigned by the target system (if any).
	MessageID string
	// Success indicates whether the send was successful.
	Success bool
	// Error contains any error that occurred.
	Error error
	// Metadata contains any additional result metadata from the target.
	Metadata map[string]any
}


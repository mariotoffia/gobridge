package types

import (
	"context"
	"io"
)

// PipelineMode defines whether a pipeline is simplex (one-way) or duplex (bidirectional).
type PipelineMode string

const (
	// PipelineModeSimplex is a one-way pipeline (source → target).
	PipelineModeSimplex PipelineMode = "simplex"
	// PipelineModeDuplex is a bidirectional pipeline (source ↔ target).
	// In duplex mode, the target can also act as a source and vice versa.
	PipelineModeDuplex PipelineMode = "duplex"
)

// Pipeline represents a message flow from a Source through Middlewares to a Target.
// This is the core abstraction for routing messages between different transports.
//
// Example flows:
//   - SQS (source) → Transform → Log → MQTT (target)
//   - MQTT subscription (source) → Filter → Azure Service Bus (target)
type Pipeline interface {
	io.Closer
	// GetID returns the unique identifier of the pipeline.
	GetID() string
	// GetMode returns whether this pipeline is simplex or duplex.
	GetMode() PipelineMode
	// Start begins processing messages from source to target.
	// The pipeline runs until the context is cancelled or Close is called.
	Start(ctx context.Context) error
	// Source returns the message source for this pipeline.
	Source() Source
	// Target returns the message target for this pipeline.
	Target() Target
	// Middlewares returns the middleware chain for this pipeline.
	Middlewares() *MiddlewareChain
}

// PipelineConfig defines configuration for creating a Pipeline.
type PipelineConfig interface {
	Config
	// GetMode returns the pipeline mode (simplex/duplex).
	GetMode() PipelineMode
	// GetSourceConfig returns the source configuration.
	GetSourceConfig() SourceConfig
	// GetTargetConfig returns the target configuration.
	GetTargetConfig() TargetConfig
	// GetMiddlewareNames returns the names of middlewares to apply (in order).
	GetMiddlewareNames() []string
	// GetErrorHandling returns error handling configuration.
	GetErrorHandling() *PipelineErrorConfig
	// GetConnectionID returns the ID of a shared Connection to use.
	// If empty, Source and Target are created independently via their factories.
	// If set, Source and Target are created from the Connection's providers.
	GetConnectionID() string
	// GetFlowControl returns pipeline-specific flow control configuration.
	// Returns nil to use bridge defaults.
	//
	// Flow control includes:
	//   - MaxInFlight: Backpressure limit for concurrent messages
	//   - DefaultMessageTTL: TTL for messages without explicit TTL
	GetFlowControl() *FlowControlConfig
}

// PipelineErrorConfig defines how errors are handled in a pipeline.
type PipelineErrorConfig struct {
	// MaxRetries is the maximum number of retry attempts (0 = no retries).
	MaxRetries int `json:"maxRetries,omitempty"`
	// RetryBackoff is the initial backoff duration between retries.
	RetryBackoff string `json:"retryBackoff,omitempty"` // e.g., "1s", "500ms"
	// DeadLetterTarget is the target ID for failed messages (optional).
	DeadLetterTarget string `json:"deadLetterTarget,omitempty"`
	// DropOnPermanentError drops messages that fail with permanent errors.
	DropOnPermanentError bool `json:"dropOnPermanentError,omitempty"`
}

// Route represents a chain of Pipelines where the output of one can be
// the input of another. This enables complex message flows.
//
// Example:
//
//	SQS → MQTT topic A → Azure Service Bus
//
// This would be modeled as two pipelines in a route:
//  1. SQS (source) → MQTT topic A (target)
//  2. MQTT topic A (source) → Azure Service Bus (target)
type Route interface {
	io.Closer
	// GetID returns the unique identifier of the route.
	GetID() string
	// Pipelines returns all pipelines in this route, in order.
	Pipelines() []Pipeline
	// Start starts all pipelines in the route.
	Start(ctx context.Context) error
}

// RouteConfig defines configuration for creating a Route.
type RouteConfig interface {
	Config
	// GetPipelineConfigs returns configurations for all pipelines in order.
	GetPipelineConfigs() []PipelineConfig
}

// PipelineFactory creates Pipeline instances.
type PipelineFactory interface {
	// CreatePipeline creates a new Pipeline from the given configuration.
	CreatePipeline(ctx context.Context, config PipelineConfig) (Pipeline, error)
}

// RouteFactory creates Route instances.
type RouteFactory interface {
	// CreateRoute creates a new Route from the given configuration.
	CreateRoute(ctx context.Context, config RouteConfig) (Route, error)
}

// PipelineStats provides runtime statistics for a pipeline.
type PipelineStats struct {
	// MessagesReceived is the total number of messages received from source.
	MessagesReceived int64
	// MessagesSent is the total number of messages successfully sent to target.
	MessagesSent int64
	// MessagesFailed is the total number of messages that failed processing.
	MessagesFailed int64
	// MessagesRetried is the total number of retry attempts.
	MessagesRetried int64
	// MessagesDropped is the total number of messages dropped (permanent failures).
	MessagesDropped int64
	// InFlight is the current number of messages being processed.
	InFlight int64
}

// StatsProvider is implemented by pipelines that expose statistics.
type StatsProvider interface {
	// Stats returns current pipeline statistics.
	Stats() PipelineStats
}

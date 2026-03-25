package types

import (
	"context"
	"io"
	"time"
)

// ============================================================================
// Target Interface
// ============================================================================

// Target represents a message destination that sends messages to an external system.
// Examples: MQTT publish, SQS send, Azure Service Bus send, Kafka producer.
//
// A Target is typically the egress point of a Pipeline.
//
// # Implementor Contract
//
// Target implementations MUST follow these rules for the Send() method:
//
// ## Return Values
//
//   - nil: Message ACCEPTED by target system. The target system now owns delivery.
//     The caller MUST NOT retry. The message lifecycle is complete.
//   - error: Message NOT accepted. The error MUST be a *BridgeError with correct
//     Code and IsRecoverable fields to enable proper pipeline error handling.
//
// ## Error Classification
//
// Implementations MUST return errors with correct IsRecoverable classification:
//
//	Recoverable (IsRecoverable=true) - Pipeline will retry via RetryManager:
//	  - ErrTimeout: Request timed out waiting for acknowledgment
//	  - ErrConnectionLost: Connection dropped during send
//	  - ErrUnavailable: Target service temporarily unavailable
//	  - ErrThrottled: Rate limited (use WithRetryAfter for delay hint)
//	  - ErrBrokerBusy: Target overloaded
//
//	Permanent (IsRecoverable=false) - Pipeline will archive to DeadLetterQueue:
//	  - ErrNotAuthorized: Authentication failed
//	  - ErrForbidden: Permission denied
//	  - ErrNotFound: Target resource does not exist
//	  - ErrInvalidPayload: Message payload is malformed
//	  - ErrPayloadTooLarge: Message exceeds size limit
//	  - ErrInvalidTopic: Topic/destination format invalid
//	  - ErrProtocolError: Protocol violation
//
// ## Example Implementation
//
//	func (t *MyTarget) Send(ctx context.Context, msg Message) error {
//	    // Attempt to send
//	    err := t.client.Publish(ctx, msg.Topic, msg.Payload)
//	    if err == nil {
//	        return nil // Success - target owns delivery now
//	    }
//
//	    // Map transport-specific error to BridgeError
//	    if isTimeout(err) {
//	        return ErrTimeout.Wrap(err)
//	    }
//	    if isAuthError(err) {
//	        return ErrNotAuthorized.Wrap(err)
//	    }
//	    // Default: treat unknown errors as recoverable
//	    return ErrUnavailable.Wrap(err)
//	}
type Target interface {
	io.Closer

	// GetID returns the unique identifier of the target.
	GetID() string

	// GetTransportType returns the transport type (e.g., "MQTT", "SQS").
	GetTransportType() TransportType

	// Send sends a message to the target destination.
	//
	// Return values:
	//   - nil: Message ACCEPTED by target. Target system now owns delivery.
	//          Caller MUST NOT retry. Message lifecycle complete.
	//   - error: Message NOT accepted. Must be *BridgeError with correct
	//            IsRecoverable field. See Target interface documentation.
	//
	// The topic/queue is typically embedded in the message or configured
	// in the target.
	Send(ctx context.Context, msg Message) error

	// SendBatch sends multiple messages in a single operation if supported.
	// Returns the number of successfully sent messages and any error.
	// If batching is not supported, it sends messages sequentially.
	//
	// On partial failure:
	//   - sent: Number of messages successfully sent (target owns these)
	//   - err: Error for the first failed message (remaining not attempted)
	//
	// The caller should only retry messages at index >= sent.
	SendBatch(ctx context.Context, msgs []Message) (sent int, err error)

	// Capabilities returns the capabilities of this target.
	// Use this to query what features the target supports.
	Capabilities() Capabilities
}

// ============================================================================
// Target Configuration
// ============================================================================

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

// ============================================================================
// Target Factory and Registry
// ============================================================================

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

// ============================================================================
// Target Result
// ============================================================================

// TargetResult represents the result of sending a message to a target.
// Used for detailed tracking in pipelines and batch operations.
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

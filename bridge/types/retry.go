package types

import (
	"context"
	"time"
)

// ============================================================================
// Retry Manager
// ============================================================================

// RetryManager handles message retry logic.
// The implementation can be backed by various technologies:
//   - SQS (messages return to queue automatically)
//   - Redis (for distributed retry with persistence)
//   - In-memory (for simple cases)
//
// The retry manager is typically used by middleware or pipelines to handle
// recoverable errors.
type RetryManager interface {
	// Enqueue adds a message to the retry queue.
	// The reason describes why the message needs to be retried.
	// Returns immediately; actual retry happens asynchronously.
	Enqueue(ctx context.Context, msg Message, reason error) error

	// Start begins processing retries. Retried messages are sent to the handler.
	// Blocks until context is cancelled.
	Start(ctx context.Context, handler Subscriber) error

	// Stats returns current retry statistics.
	Stats() RetryStats

	// Purge removes all messages from the retry queue.
	Purge(ctx context.Context) error
}

// RetryStats provides statistics about the retry manager.
type RetryStats struct {
	// Pending is the number of messages waiting to be retried.
	Pending int64 `json:"pending"`
	// InFlight is the number of messages currently being retried.
	InFlight int64 `json:"inFlight"`
	// Succeeded is the total number of successful retries.
	Succeeded int64 `json:"succeeded"`
	// Failed is the total number of failed retries (exhausted).
	Failed int64 `json:"failed"`
	// TotalAttempts is the total number of retry attempts.
	TotalAttempts int64 `json:"totalAttempts"`
}

// RetryPolicy defines how retries should be performed.
type RetryPolicy struct {
	// MaxAttempts is the maximum number of retry attempts (0 = unlimited).
	MaxAttempts int `json:"maxAttempts,omitempty"`
	// InitialBackoff is the initial delay before first retry.
	InitialBackoff time.Duration `json:"initialBackoff,omitempty"`
	// MaxBackoff is the maximum delay between retries.
	MaxBackoff time.Duration `json:"maxBackoff,omitempty"`
	// BackoffMultiplier is the factor by which backoff increases.
	BackoffMultiplier float64 `json:"backoffMultiplier,omitempty"`
	// Jitter adds randomness to backoff to prevent thundering herd.
	Jitter float64 `json:"jitter,omitempty"`
	// RetryableErrors defines which error codes should trigger retries.
	// If empty, all recoverable errors trigger retries.
	RetryableErrors []ErrorCode `json:"retryableErrors,omitempty"`
}

// DefaultRetryPolicy returns a sensible default retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:       3,
		InitialBackoff:    time.Second,
		MaxBackoff:        30 * time.Second,
		BackoffMultiplier: 2.0,
		Jitter:            0.1,
	}
}

// RetryInfo contains information about a message's retry state.
// This is typically stored in message metadata.
type RetryInfo struct {
	// Attempt is the current attempt number (1-based).
	Attempt int `json:"attempt"`
	// MaxAttempts is the maximum attempts configured.
	MaxAttempts int `json:"maxAttempts"`
	// FirstAttemptAt is when the first attempt was made.
	FirstAttemptAt time.Time `json:"firstAttemptAt"`
	// LastAttemptAt is when the last attempt was made.
	LastAttemptAt time.Time `json:"lastAttemptAt"`
	// LastError is the error from the last attempt.
	LastError string `json:"lastError,omitempty"`
	// NextRetryAt is when the next retry will be attempted.
	NextRetryAt time.Time `json:"nextRetryAt,omitempty"`
}

// IsExhausted returns true if all retry attempts have been used.
func (r *RetryInfo) IsExhausted() bool {
	return r.MaxAttempts > 0 && r.Attempt >= r.MaxAttempts
}

// ============================================================================
// Dead Letter Queue
// ============================================================================

// DeadLetterQueue stores messages that could not be processed.
// This is the final destination for messages that exhaust all retries
// or encounter permanent errors.
type DeadLetterQueue interface {
	// Send moves a message to the dead letter queue.
	// The reason describes why the message was rejected.
	Send(ctx context.Context, msg Message, reason error) error

	// Consume returns a channel of messages from the DLQ.
	// Used for reprocessing or inspection.
	Consume(ctx context.Context) (<-chan *DLQMessage, error)

	// Count returns the number of messages in the DLQ.
	Count(ctx context.Context) (int64, error)

	// Purge removes all messages from the DLQ.
	Purge(ctx context.Context) error

	// Replay moves messages from DLQ back to the retry queue.
	// Filter can be used to select which messages to replay.
	Replay(ctx context.Context, filter DLQFilter) (replayed int64, err error)
}

// DLQMessage wraps a message with DLQ-specific information.
type DLQMessage struct {
	// Message is the original message.
	Message Message
	// Reason is why the message was moved to DLQ.
	Reason string `json:"reason"`
	// FailedAt is when the message was moved to DLQ.
	FailedAt time.Time `json:"failedAt"`
	// RetryInfo contains retry history if available.
	RetryInfo *RetryInfo `json:"retryInfo,omitempty"`
	// SourceID identifies where the message came from.
	SourceID string `json:"sourceId,omitempty"`
}

// DLQFilter defines criteria for filtering DLQ messages.
type DLQFilter struct {
	// Topic filters by original topic.
	Topic string `json:"topic,omitempty"`
	// Since filters to messages failed after this time.
	Since time.Time `json:"since,omitempty"`
	// Until filters to messages failed before this time.
	Until time.Time `json:"until,omitempty"`
	// SourceID filters by source.
	SourceID string `json:"sourceId,omitempty"`
	// MaxMessages limits the number of messages to replay.
	MaxMessages int64 `json:"maxMessages,omitempty"`
}

// ============================================================================
// Retry Middleware
// ============================================================================

// RetryMiddleware creates a Middleware that handles retries.
func RetryMiddleware(name string, manager RetryManager, policy RetryPolicy) Middleware {
	return NewMiddlewareAdapter(name, func(ctx context.Context, msg *Message, next MiddlewareFunc) error {
		err := next(ctx, msg)
		if err == nil {
			return nil
		}

		// Check if error is recoverable
		var bridgeErr *BridgeError
		if be, ok := err.(*BridgeError); ok {
			bridgeErr = be
		}

		if bridgeErr != nil && !bridgeErr.IsRecoverable {
			// Permanent error - don't retry
			return err
		}

		// Check retry policy
		if len(policy.RetryableErrors) > 0 && bridgeErr != nil {
			shouldRetry := false
			for _, code := range policy.RetryableErrors {
				if bridgeErr.Code == code {
					shouldRetry = true
					break
				}
			}
			if !shouldRetry {
				return err
			}
		}

		// Enqueue for retry
		if enqErr := manager.Enqueue(ctx, *msg, err); enqErr != nil {
			// Failed to enqueue - return original error
			return err
		}

		// Message queued for retry - return nil to acknowledge to source
		return nil
	})
}


package types

import (
	"errors"
	"fmt"
	"time"
)

// ============================================================================
// Error Codes
// ============================================================================

// ErrorCode is a unique identifier for error types, enabling sentinel comparison.
// Error codes are used by the pipeline to determine whether to retry or archive
// a message. See the error classification documentation for details.
type ErrorCode string

// Recoverable error codes - these errors may succeed on retry.
// The pipeline will enqueue messages with these errors to the RetryManager.
const (
	// ErrCodeTimeout indicates a request timed out waiting for response.
	// Retry strategy: Immediate retry with backoff.
	ErrCodeTimeout ErrorCode = "TIMEOUT"

	// ErrCodeConnectionLost indicates the connection was dropped during operation.
	// Retry strategy: Retry with backoff after reconnection.
	ErrCodeConnectionLost ErrorCode = "CONNECTION_LOST"

	// ErrCodeUnavailable indicates the target service is temporarily unavailable.
	// Retry strategy: Retry with exponential backoff.
	ErrCodeUnavailable ErrorCode = "UNAVAILABLE"

	// ErrCodeThrottled indicates the request was rate-limited.
	// Retry strategy: Retry after RetryAfter duration.
	ErrCodeThrottled ErrorCode = "THROTTLED"

	// ErrCodeBrokerBusy indicates the target is overloaded.
	// Retry strategy: Retry with exponential backoff.
	ErrCodeBrokerBusy ErrorCode = "BROKER_BUSY"

	// ErrCodeTemporaryAuthFailure indicates authentication failed but may succeed
	// after credential refresh. Retry strategy: Retry after credential refresh.
	ErrCodeTemporaryAuthFailure ErrorCode = "TEMPORARY_AUTH_FAILURE"
)

// Permanent error codes - these errors will never succeed on retry.
// The pipeline will send messages with these errors directly to the DeadLetterQueue.
const (
	// ErrCodeNotAuthorized indicates authentication failed permanently.
	// Resolution: Fix credentials or permissions.
	ErrCodeNotAuthorized ErrorCode = "NOT_AUTHORIZED"

	// ErrCodeForbidden indicates the operation is not permitted.
	// Resolution: Fix permissions or access policies.
	ErrCodeForbidden ErrorCode = "FORBIDDEN"

	// ErrCodeNotFound indicates the target resource does not exist.
	// Resolution: Create the resource or fix the configuration.
	ErrCodeNotFound ErrorCode = "NOT_FOUND"

	// ErrCodeInvalidPayload indicates the message payload is malformed.
	// Resolution: Fix the message content.
	ErrCodeInvalidPayload ErrorCode = "INVALID_PAYLOAD"

	// ErrCodePayloadTooLarge indicates the message exceeds size limits.
	// Resolution: Split or compress the message.
	ErrCodePayloadTooLarge ErrorCode = "PAYLOAD_TOO_LARGE"

	// ErrCodeInvalidTopic indicates the topic/destination format is invalid.
	// Resolution: Fix the topic name.
	ErrCodeInvalidTopic ErrorCode = "INVALID_TOPIC"

	// ErrCodeProtocolError indicates a protocol violation occurred.
	// Resolution: Fix the implementation bug.
	ErrCodeProtocolError ErrorCode = "PROTOCOL_ERROR"

	// ErrCodeSchemaViolation indicates the message failed schema validation.
	// Resolution: Fix the message to match the expected schema.
	ErrCodeSchemaViolation ErrorCode = "SCHEMA_VIOLATION"

	// ErrCodeMessageExpired indicates the message expired before delivery.
	// Resolution: Check message TTL settings.
	ErrCodeMessageExpired ErrorCode = "MESSAGE_EXPIRED"

	// ErrCodeQoSNotSupported indicates the requested QoS level is not supported.
	// Resolution: Use a supported QoS level.
	ErrCodeQoSNotSupported ErrorCode = "QOS_NOT_SUPPORTED"
)

// Other error codes for specific subsystems.
const (
	// ErrCodeSubscriptionExists indicates a subscription already exists.
	ErrCodeSubscriptionExists ErrorCode = "SUBSCRIPTION_EXISTS"

	// ErrCodeNotBidirectional indicates a connection does not support the operation.
	ErrCodeNotBidirectional ErrorCode = "NOT_BIDIRECTIONAL"
)

// ============================================================================
// Sentinel Errors - Recoverable (Retry Allowed)
// ============================================================================

var (
	// ErrTimeout indicates a request timed out.
	// IsRecoverable: true - may succeed on retry.
	ErrTimeout = &BridgeError{
		Code:          ErrCodeTimeout,
		Message:       "request timed out",
		HttpCode:      504,
		IsRecoverable: true,
	}

	// ErrConnectionLost indicates the connection was dropped.
	// IsRecoverable: true - may succeed after reconnection.
	ErrConnectionLost = &BridgeError{
		Code:          ErrCodeConnectionLost,
		Message:       "connection lost",
		HttpCode:      503,
		IsRecoverable: true,
	}

	// ErrUnavailable indicates the target service is temporarily unavailable.
	// IsRecoverable: true - may succeed on retry.
	ErrUnavailable = &BridgeError{
		Code:          ErrCodeUnavailable,
		Message:       "service unavailable",
		HttpCode:      503,
		IsRecoverable: true,
	}

	// ErrThrottled indicates the request was rate-limited.
	// IsRecoverable: true - will succeed after backoff.
	// Use WithRetryAfter() to specify the retry delay.
	ErrThrottled = &BridgeError{
		Code:          ErrCodeThrottled,
		Message:       "rate limited",
		HttpCode:      429,
		IsRecoverable: true,
	}

	// ErrBrokerBusy indicates the target is overloaded.
	// IsRecoverable: true - may succeed on retry.
	ErrBrokerBusy = &BridgeError{
		Code:          ErrCodeBrokerBusy,
		Message:       "target overloaded",
		HttpCode:      503,
		IsRecoverable: true,
	}

	// ErrTemporaryAuthFailure indicates authentication failed but may recover.
	// IsRecoverable: true - may succeed after credential refresh.
	ErrTemporaryAuthFailure = &BridgeError{
		Code:          ErrCodeTemporaryAuthFailure,
		Message:       "authentication temporarily failed",
		HttpCode:      401,
		IsRecoverable: true,
	}
)

// ============================================================================
// Sentinel Errors - Permanent (No Retry - Archive to DLQ)
// ============================================================================

var (
	// ErrNotAuthorized indicates authentication failed permanently.
	// IsRecoverable: false - credentials are invalid.
	ErrNotAuthorized = &BridgeError{
		Code:          ErrCodeNotAuthorized,
		Message:       "not authorized",
		HttpCode:      401,
		IsRecoverable: false,
	}

	// ErrForbidden indicates the operation is not permitted.
	// IsRecoverable: false - permission denied.
	ErrForbidden = &BridgeError{
		Code:          ErrCodeForbidden,
		Message:       "forbidden",
		HttpCode:      403,
		IsRecoverable: false,
	}

	// ErrNotFound indicates the target resource does not exist.
	// IsRecoverable: false - resource must be created.
	ErrNotFound = &BridgeError{
		Code:          ErrCodeNotFound,
		Message:       "not found",
		HttpCode:      404,
		IsRecoverable: false,
	}

	// ErrInvalidPayload indicates the message payload is malformed.
	// IsRecoverable: false - message must be fixed.
	ErrInvalidPayload = &BridgeError{
		Code:          ErrCodeInvalidPayload,
		Message:       "invalid payload",
		HttpCode:      422,
		IsRecoverable: false,
	}

	// ErrPayloadTooLarge indicates the message exceeds size limits.
	// IsRecoverable: false - message must be split or compressed.
	ErrPayloadTooLarge = &BridgeError{
		Code:          ErrCodePayloadTooLarge,
		Message:       "payload too large",
		HttpCode:      413,
		IsRecoverable: false,
	}

	// ErrInvalidTopic indicates the topic/destination format is invalid.
	// IsRecoverable: false - topic must be fixed.
	ErrInvalidTopic = &BridgeError{
		Code:          ErrCodeInvalidTopic,
		Message:       "invalid topic",
		HttpCode:      400,
		IsRecoverable: false,
	}

	// ErrProtocolError indicates a protocol violation occurred.
	// IsRecoverable: false - implementation bug.
	ErrProtocolError = &BridgeError{
		Code:          ErrCodeProtocolError,
		Message:       "protocol error",
		HttpCode:      400,
		IsRecoverable: false,
	}

	// ErrSchemaViolation indicates the message failed schema validation.
	// IsRecoverable: false - message must match schema.
	ErrSchemaViolation = &BridgeError{
		Code:          ErrCodeSchemaViolation,
		Message:       "schema validation failed",
		HttpCode:      422,
		IsRecoverable: false,
	}

	// ErrMessageExpired indicates the message expired before delivery.
	// IsRecoverable: false - message is stale.
	ErrMessageExpired = &BridgeError{
		Code:          ErrCodeMessageExpired,
		Message:       "message expired",
		HttpCode:      410,
		IsRecoverable: false,
	}

	// ErrQoSNotSupported indicates the requested QoS level is not supported.
	// IsRecoverable: false - configuration must be changed.
	ErrQoSNotSupported = &BridgeError{
		Code:          ErrCodeQoSNotSupported,
		Message:       "QoS level not supported",
		HttpCode:      400,
		IsRecoverable: false,
	}

	// ErrSubscriptionExists indicates a subscription already exists.
	// IsRecoverable: false - subscription must be removed first.
	ErrSubscriptionExists = &BridgeError{
		Code:          ErrCodeSubscriptionExists,
		Message:       "subscription already exists",
		HttpCode:      409,
		IsRecoverable: false,
	}

	// ErrNotBidirectional indicates the connection does not support the operation.
	// IsRecoverable: false - wrong connection type.
	ErrNotBidirectional = &BridgeError{
		Code:          ErrCodeNotBidirectional,
		Message:       "connection is not bidirectional",
		HttpCode:      400,
		IsRecoverable: false,
	}
)

// ============================================================================
// BridgeError Type
// ============================================================================

// BridgeError is the structured error type for the bridge system.
// All transport implementations MUST return BridgeError from Send() and other
// operations to enable proper error classification by the pipeline.
//
// Error Classification Rules:
//   - IsRecoverable=true: Pipeline will enqueue to RetryManager for retry
//   - IsRecoverable=false: Pipeline will archive to DeadLetterQueue immediately
//
// Example usage:
//
//	// Wrap an underlying error with classification
//	return ErrConnectionLost.Wrap(err)
//
//	// Add context for debugging
//	return ErrTimeout.With("operation", "publish").With("topic", topic).Wrap(err)
//
//	// Use WithRetryAfter for throttling
//	return ErrThrottled.WithRetryAfter(5 * time.Second).Wrap(err)
type BridgeError struct {
	// Code uniquely identifies the error type for sentinel comparison.
	Code ErrorCode

	// Message is the human-readable description of the error.
	Message string

	// Wrapped is the underlying error (if any).
	Wrapped error

	// HttpCode is optional HTTP status code associated with the error.
	HttpCode int

	// IsRecoverable indicates whether this error is retryable (true) or permanent (false).
	// The pipeline uses this to decide: retry via RetryManager or archive to DLQ.
	IsRecoverable bool

	// RetryAfter hints how long to wait before retrying (for throttling).
	RetryAfter time.Duration

	// Context holds dynamic key-value pairs for additional error context.
	Context map[string]any
}

// Error implements the error interface.
func (e *BridgeError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Wrapped)
	}
	return e.Message
}

// Unwrap returns the wrapped error for errors.Unwrap() compatibility.
func (e *BridgeError) Unwrap() error {
	return e.Wrapped
}

// Is enables errors.Is() comparison using error codes.
// Two BridgeErrors are equal if they have the same Code.
func (e *BridgeError) Is(target error) bool {
	t, ok := target.(*BridgeError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// IsHttpCodeSet returns true if an HTTP code is set.
func (e *BridgeError) IsHttpCodeSet() bool {
	return e.HttpCode != 0
}

// With returns a copy of the error with additional context.
// This allows adding dynamic information without mutating the original sentinel.
func (e *BridgeError) With(key string, value any) *BridgeError {
	clone := *e
	if clone.Context == nil {
		clone.Context = make(map[string]any)
	} else {
		// Copy existing context
		newCtx := make(map[string]any, len(e.Context)+1)
		for k, v := range e.Context {
			newCtx[k] = v
		}
		clone.Context = newCtx
	}
	clone.Context[key] = value
	return &clone
}

// WithMessage returns a copy with a custom message while preserving the error code.
func (e *BridgeError) WithMessage(msg string) *BridgeError {
	clone := *e
	clone.Message = msg
	return &clone
}

// Wrap returns a copy with the wrapped error set.
func (e *BridgeError) Wrap(err error) *BridgeError {
	clone := *e
	clone.Wrapped = err
	return &clone
}

// WithRetryAfter returns a copy with a retry delay hint.
// Use this for throttling errors to indicate when to retry.
func (e *BridgeError) WithRetryAfter(d time.Duration) *BridgeError {
	clone := *e
	clone.RetryAfter = d
	return &clone
}

// GetContext retrieves a value from the error context.
func (e *BridgeError) GetContext(key string) (any, bool) {
	if e.Context == nil {
		return nil, false
	}
	v, ok := e.Context[key]
	return v, ok
}

// ============================================================================
// Helper Functions
// ============================================================================

// NewBridgeError creates a new BridgeError with the given parameters.
func NewBridgeError(code ErrorCode, message string, isRecoverable bool, httpCode ...int) *BridgeError {
	var hc int
	if len(httpCode) > 0 {
		hc = httpCode[0]
	}
	return &BridgeError{
		Code:          code,
		Message:       message,
		HttpCode:      hc,
		IsRecoverable: isRecoverable,
	}
}

// NewBridgeErrorWrapped creates a new BridgeError with a wrapped error.
func NewBridgeErrorWrapped(code ErrorCode, message string, wrapped error, isRecoverable bool, httpCode int) *BridgeError {
	return &BridgeError{
		Code:          code,
		Message:       message,
		Wrapped:       wrapped,
		HttpCode:      httpCode,
		IsRecoverable: isRecoverable,
	}
}

// IsBridgeError checks if an error matches a sentinel BridgeError.
func IsBridgeError(err error, target *BridgeError) bool {
	return errors.Is(err, target)
}

// AsBridgeError attempts to extract a BridgeError from an error chain.
// Returns the BridgeError and true if found, nil and false otherwise.
func AsBridgeError(err error) (*BridgeError, bool) {
	var be *BridgeError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}

// IsRecoverableError checks if an error is recoverable.
// For non-BridgeError types, returns true (safe default - retry unknown errors).
func IsRecoverableError(err error) bool {
	if err == nil {
		return false
	}
	be, ok := AsBridgeError(err)
	if !ok {
		// Unknown error type - treat as recoverable to be safe
		return true
	}
	return be.IsRecoverable
}

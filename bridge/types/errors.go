package types

import (
	"errors"
	"fmt"
	"time"
)

var (
	//
	// Temporary / retryable (recoverable) errors
	//
	ErrServerUnavailable  = NewBridgeError(ErrCodeServerUnavailable, "server unavailable", true, 503)
	ErrNetworkUnavailable = NewBridgeError(ErrCodeNetworkUnavailable, "network unavailable", true, 503)
	ErrBrokerOverload     = NewBridgeError(ErrCodeBrokerOverload, "broker overloaded", true, 503)
	ErrPublishTimeout     = NewBridgeError(ErrCodePublishTimeout, "publish timeout", true, 504)
	ErrBackoff            = NewBackoffError("Backoff in effect", 30)
	// ErrTemporaryAuthFailed is returned when authentication or authorization fails temporarily. This can be
	// e.g. when publishing messages and the credentials may be reconfigured to make subsequent attempts succeed.
	ErrTemporaryAuthFailed = NewBridgeError(ErrCodeTemporaryAuthFailed, "authentication/authorization failed", true, 401)

	//
	// Permanent / non-recoverable errors
	//

	ErrServerNotConnected    = NewBridgeError(ErrCodeServerNotConnected, "server not connected", false, 502)
	ErrTopicDoesNotExist     = NewBridgeError(ErrCodeTopicDoesNotExist, "topic does not exist", false, 404)
	ErrInvalidTopicName      = NewBridgeError(ErrCodeInvalidTopicName, "invalid topic name", false, 400)
	ErrQoSNotSupported       = NewBridgeError(ErrCodeQoSNotSupported, "QoS level not supported", false, 400)
	ErrPayloadTooLarge       = NewBridgeError(ErrCodePayloadTooLarge, "payload too large", false, 413)
	ErrInvalidPayload        = NewBridgeError(ErrCodeInvalidPayload, "invalid payload", false, 422)
	ErrPermanentAuthFailed   = NewBridgeError(ErrCodePermanentAuthFailed, "authentication/authorization failed", false, 401)
	ErrPublishDeniedByBroker = NewBridgeError(ErrCodePublishDeniedByBroker, "publish denied by broker policy", false, 403)
	ErrProtocolMismatch      = NewBridgeError(ErrCodeProtocolMismatch, "protocol version or feature not supported", false, 400)
	ErrMessageExpired        = NewBridgeError(ErrCodeMessageExpired, "message expired before delivery", false, 410)

	//
	// Generic errors
	//
	ErrNotFound = NewBridgeError(ErrCodeNotFound, "not found", false, 404)

	//
	// Subscriber related errors
	//
	ErrSubscriptionAlreadyExists    = NewBridgeError(ErrCodeSubscriptionExists, "subscriber already exists for topic", false, 409)
	ErrSubscriptionInvalidTopicName = NewBridgeError(ErrCodeInvalidSubscriptionName, "topic is not a valid topic", false, 400)
	//
	// Connection related errors
	//
	ConnectionNotBidirectionalError = NewBridgeError(ErrCodeNotBidirectional, "connection is not bidirectional", false, 400)
)

// ErrorCode is a unique identifier for error types, enabling sentinel comparison.
type ErrorCode string

const (
	ErrCodeServerUnavailable       ErrorCode = "SERVER_UNAVAILABLE"
	ErrCodeNetworkUnavailable      ErrorCode = "NETWORK_UNAVAILABLE"
	ErrCodeBrokerOverload          ErrorCode = "BROKER_OVERLOAD"
	ErrCodePublishTimeout          ErrorCode = "PUBLISH_TIMEOUT"
	ErrCodeBackoff                 ErrorCode = "BACKOFF"
	ErrCodeTemporaryAuthFailed     ErrorCode = "TEMPORARY_AUTH_FAILED"
	ErrCodeServerNotConnected      ErrorCode = "SERVER_NOT_CONNECTED"
	ErrCodeTopicDoesNotExist       ErrorCode = "TOPIC_NOT_EXIST"
	ErrCodeInvalidTopicName        ErrorCode = "INVALID_TOPIC_NAME"
	ErrCodeQoSNotSupported         ErrorCode = "QOS_NOT_SUPPORTED"
	ErrCodePayloadTooLarge         ErrorCode = "PAYLOAD_TOO_LARGE"
	ErrCodeInvalidPayload          ErrorCode = "INVALID_PAYLOAD"
	ErrCodePermanentAuthFailed     ErrorCode = "PERMANENT_AUTH_FAILED"
	ErrCodePublishDeniedByBroker   ErrorCode = "PUBLISH_DENIED"
	ErrCodeProtocolMismatch        ErrorCode = "PROTOCOL_MISMATCH"
	ErrCodeMessageExpired          ErrorCode = "MESSAGE_EXPIRED"
	ErrCodeNotFound                ErrorCode = "NOT_FOUND"
	ErrCodeSubscriptionExists      ErrorCode = "SUBSCRIPTION_EXISTS"
	ErrCodeInvalidSubscriptionName ErrorCode = "INVALID_SUBSCRIPTION_NAME"
	ErrCodeNotBidirectional        ErrorCode = "NOT_BIDIRECTIONAL"
)

// BridgeError is a structured error type for the bridge system.
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
	IsRecoverable bool
	// Context holds dynamic key-value pairs for additional error context.
	Context map[string]any
}

func (e *BridgeError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Wrapped)
	}
	return e.Message
}

func (e *BridgeError) Unwrap() error {
	return e.Wrapped
}

// Is enables errors.Is() comparison using error codes.
func (e *BridgeError) Is(target error) bool {
	t, ok := target.(*BridgeError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

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

// GetContext retrieves a value from the error context.
func (e *BridgeError) GetContext(key string) (any, bool) {
	if e.Context == nil {
		return nil, false
	}
	v, ok := e.Context[key]
	return v, ok
}

// constructor for a non-wrapped error
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

// constructor for wrapped error
func NewBridgeErrorWrapped(code ErrorCode, message string, wrapped error, isRecoverable bool, httpCode int) *BridgeError {
	return &BridgeError{
		Code:          code,
		Message:       message,
		Wrapped:       wrapped,
		HttpCode:      httpCode,
		IsRecoverable: isRecoverable,
	}
}

// Helper to check for sentinel
func IsBridgeError(err error, target *BridgeError) bool {
	return errors.Is(err, target)
}

type BackoffError struct {
	*BridgeError
	RetryAfter time.Duration
}

func NewBackoffError(msg string, retryAfterSeconds int) *BackoffError {
	return &BackoffError{
		BridgeError: NewBridgeError(ErrCodeBackoff, msg, true, 429),
		RetryAfter:  time.Duration(retryAfterSeconds) * time.Second,
	}
}

// WithRetryAfter returns a copy with a custom retry duration.
func (e *BackoffError) WithRetryAfter(d time.Duration) *BackoffError {
	clone := *e
	clone.RetryAfter = d
	return &clone
}

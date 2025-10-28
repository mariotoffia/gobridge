package types

import (
	"errors"
	"fmt"
)

var (
	//
	// Temporary / retryable (recoverable) errors
	//
	ErrServerUnavailable  = NewBridgeError("server unavailable", true, 503)
	ErrNetworkUnavailable = NewBridgeError("network unavailable", true, 503)
	ErrBrokerOverload     = NewBridgeError("broker overloaded", true, 503)
	ErrPublishTimeout     = NewBridgeError("publish timeout", true, 504)
	ErrBackoff            = NewBackoffError("Backoff in effect", 30)
	// ErrTemporaryAuthFailed is returned when authentication or authorization fails temporarily. This can be
	// e.g. when publishing messages and the credentials may be reconfigured to make subsequent attempts succeed.
	ErrTemporaryAuthFailed = NewBridgeError("authentication/authorization failed", true, 401)

	//
	// Permanent / non-recoverable errors
	//

	ErrServerNotConnected    = NewBridgeError("server not connected", false, 502)
	ErrTopicDoesNotExist     = NewBridgeError("topic does not exist", false, 404)
	ErrInvalidTopicName      = NewBridgeError("invalid topic name", false, 400)
	ErrQoSNotSupported       = NewBridgeError("QoS level not supported", false, 400)
	ErrPayloadTooLarge       = NewBridgeError("payload too large", false, 413)
	ErrInvalidPayload        = NewBridgeError("invalid payload", false, 422)
	ErrPermanentAuthFailed   = NewBridgeError("authentication/authorization failed", false, 401)
	ErrPublishDeniedByBroker = NewBridgeError("publish denied by broker policy", false, 403)
	ErrProtocolMismatch      = NewBridgeError("protocol version or feature not supported", false, 400)
	ErrMessageExpired        = NewBridgeError("message expired before delivery", false, 410)

	//
	// Generic errors
	//
	ErrNotFound = NewBridgeError("not found", false, 404)

	//
	// Subscriber related errors
	//
	ErrSubscriptionAlreadyExists    = NewBridgeError("subscriber already exists for topic", false, 409)
	ErrSubscriptionInvalidTopicName = NewBridgeError("topic is not a valid topic", false, 400)
	//
	// Connection related errors
	//
	ConnectionNotBidirectionalError = NewBridgeError("connection is not bidirectional", false, 400)
)

type BridgeError struct {
	// Message is the human-readable description of the error.
	Message string
	// Wrapped is the underlying error (if any).
	Wrapped error
	// HttpCode is optional HTTP status code associated with the error.
	HttpCode int
	// IsRecoverable indicates whether this error is retryable (true) or permanent (false).
	IsRecoverable bool
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

func (e *BridgeError) IsHttpCodeSet() bool {
	return e.HttpCode != 0
}

// constructor for a non-wrapped error
func NewBridgeError(message string, isRecoverable bool, httpCode ...int) *BridgeError {
	var code int
	if len(httpCode) > 0 {
		code = httpCode[0]
	}
	return &BridgeError{
		Message:       message,
		HttpCode:      code,
		IsRecoverable: isRecoverable,
	}
}

// constructor for wrapped error
func NewBridgeErrorWrapped(message string, wrapped error, isRecoverable bool, httpCode int) *BridgeError {
	return &BridgeError{
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
	RetryAfterSeconds int
}

func NewBackoffError(msg string, retryAfterSeconds int) *BackoffError {
	return &BackoffError{
		BridgeError:       NewBridgeError(msg, true, 429),
		RetryAfterSeconds: retryAfterSeconds,
	}
}

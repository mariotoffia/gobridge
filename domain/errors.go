package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrorClass classifies errors for routing decisions in the runtime.
type ErrorClass string

const (
	ErrorTransient ErrorClass = "transient"
	ErrorPermanent ErrorClass = "permanent"
	ErrorExpired   ErrorClass = "expired"
	ErrorRejected  ErrorClass = "rejected"
)

// ErrorCode uniquely identifies an error type for sentinel comparison.
type ErrorCode string

// Recoverable error codes -- may succeed on retry.
const (
	ErrCodeTimeout              ErrorCode = "TIMEOUT"
	ErrCodeConnectionLost       ErrorCode = "CONNECTION_LOST"
	ErrCodeUnavailable          ErrorCode = "UNAVAILABLE"
	ErrCodeThrottled            ErrorCode = "THROTTLED"
	ErrCodeBrokerBusy           ErrorCode = "BROKER_BUSY"
	ErrCodeTemporaryAuthFailure ErrorCode = "TEMPORARY_AUTH_FAILURE"
)

// Permanent error codes -- retry will not help.
const (
	ErrCodeNotAuthorized   ErrorCode = "NOT_AUTHORIZED"
	ErrCodeForbidden       ErrorCode = "FORBIDDEN"
	ErrCodeNotFound        ErrorCode = "NOT_FOUND"
	ErrCodeInvalidPayload  ErrorCode = "INVALID_PAYLOAD"
	ErrCodePayloadTooLarge ErrorCode = "PAYLOAD_TOO_LARGE"
	ErrCodeInvalidTopic    ErrorCode = "INVALID_TOPIC"
	ErrCodeProtocolError   ErrorCode = "PROTOCOL_ERROR"
	ErrCodeSchemaViolation ErrorCode = "SCHEMA_VIOLATION"
	ErrCodeMessageExpired  ErrorCode = "MESSAGE_EXPIRED"
	ErrCodeQoSNotSupported ErrorCode = "QOS_NOT_SUPPORTED"
	ErrCodeMessageFiltered ErrorCode = "MESSAGE_FILTERED"
)

// Infrastructure and fencing error codes.
const (
	ErrCodeNotSupported     ErrorCode = "NOT_SUPPORTED"
	ErrCodeVersionMismatch  ErrorCode = "VERSION_MISMATCH"
	ErrCodeAlreadyExists    ErrorCode = "ALREADY_EXISTS"
	ErrCodeStaleFencingToken ErrorCode = "STALE_FENCING_TOKEN"
	ErrCodeDuplicateRecord  ErrorCode = "DUPLICATE_RECORD"
)

// BridgeError is the structured error type for the bridge.
// Transport adapters and runtime components return BridgeError instances
// so the pipeline can classify failures for retry, DLQ, or drop decisions.
type BridgeError struct {
	Code       ErrorCode
	Class      ErrorClass
	Message    string
	Cause      error
	RetryAfter time.Duration
	Context    map[string]any
}

func (e *BridgeError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *BridgeError) Unwrap() error {
	return e.Cause
}

// Is enables errors.Is() comparison by error code.
func (e *BridgeError) Is(target error) bool {
	t, ok := target.(*BridgeError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// With returns a copy with additional context.
func (e *BridgeError) With(key string, value any) *BridgeError {
	clone := e.clone()
	if clone.Context == nil {
		clone.Context = make(map[string]any, 1)
	}
	clone.Context[key] = value
	return clone
}

// WithMessage returns a copy with a custom message.
func (e *BridgeError) WithMessage(msg string) *BridgeError {
	clone := e.clone()
	clone.Message = msg
	return clone
}

// Wrap returns a copy with the cause set.
func (e *BridgeError) Wrap(err error) *BridgeError {
	clone := e.clone()
	clone.Cause = err
	return clone
}

// WithRetryAfter returns a copy with a retry delay hint.
func (e *BridgeError) WithRetryAfter(d time.Duration) *BridgeError {
	clone := e.clone()
	clone.RetryAfter = d
	return clone
}

func (e *BridgeError) clone() *BridgeError {
	c := *e
	if e.Context != nil {
		c.Context = make(map[string]any, len(e.Context))
		for k, v := range e.Context {
			c.Context[k] = v
		}
	}
	return &c
}

// Sentinel errors -- recoverable (transient).
var (
	ErrTimeout = &BridgeError{
		Code: ErrCodeTimeout, Class: ErrorTransient,
		Message: "request timed out",
	}
	ErrConnectionLost = &BridgeError{
		Code: ErrCodeConnectionLost, Class: ErrorTransient,
		Message: "connection lost",
	}
	ErrUnavailable = &BridgeError{
		Code: ErrCodeUnavailable, Class: ErrorTransient,
		Message: "service unavailable",
	}
	ErrThrottled = &BridgeError{
		Code: ErrCodeThrottled, Class: ErrorTransient,
		Message: "rate limited",
	}
	ErrBrokerBusy = &BridgeError{
		Code: ErrCodeBrokerBusy, Class: ErrorTransient,
		Message: "broker overloaded",
	}
	ErrTemporaryAuthFailure = &BridgeError{
		Code: ErrCodeTemporaryAuthFailure, Class: ErrorTransient,
		Message: "authentication temporarily failed",
	}
)

// Sentinel errors -- permanent.
var (
	ErrNotAuthorized = &BridgeError{
		Code: ErrCodeNotAuthorized, Class: ErrorPermanent,
		Message: "not authorized",
	}
	ErrForbidden = &BridgeError{
		Code: ErrCodeForbidden, Class: ErrorPermanent,
		Message: "forbidden",
	}
	ErrNotFound = &BridgeError{
		Code: ErrCodeNotFound, Class: ErrorPermanent,
		Message: "not found",
	}
	ErrInvalidPayload = &BridgeError{
		Code: ErrCodeInvalidPayload, Class: ErrorRejected,
		Message: "invalid payload",
	}
	ErrPayloadTooLarge = &BridgeError{
		Code: ErrCodePayloadTooLarge, Class: ErrorRejected,
		Message: "payload too large",
	}
	ErrInvalidTopic = &BridgeError{
		Code: ErrCodeInvalidTopic, Class: ErrorRejected,
		Message: "invalid topic",
	}
	ErrProtocolError = &BridgeError{
		Code: ErrCodeProtocolError, Class: ErrorPermanent,
		Message: "protocol error",
	}
	ErrSchemaViolation = &BridgeError{
		Code: ErrCodeSchemaViolation, Class: ErrorRejected,
		Message: "schema validation failed",
	}
	ErrMessageExpired = &BridgeError{
		Code: ErrCodeMessageExpired, Class: ErrorExpired,
		Message: "message expired",
	}
	ErrQoSNotSupported = &BridgeError{
		Code: ErrCodeQoSNotSupported, Class: ErrorPermanent,
		Message: "QoS level not supported",
	}
	ErrMessageFiltered = &BridgeError{
		Code: ErrCodeMessageFiltered, Class: ErrorRejected,
		Message: "message filtered",
	}
)

// Sentinel errors -- infrastructure / fencing.
var (
	ErrNotSupported = &BridgeError{
		Code: ErrCodeNotSupported, Class: ErrorPermanent,
		Message: "operation not supported",
	}
	ErrVersionMismatch = &BridgeError{
		Code: ErrCodeVersionMismatch, Class: ErrorPermanent,
		Message: "version mismatch",
	}
	ErrAlreadyExists = &BridgeError{
		Code: ErrCodeAlreadyExists, Class: ErrorPermanent,
		Message: "resource already exists",
	}
	ErrStaleFencingToken = &BridgeError{
		Code: ErrCodeStaleFencingToken, Class: ErrorPermanent,
		Message: "stale fencing token",
	}
	ErrDuplicateRecord = &BridgeError{
		Code: ErrCodeDuplicateRecord, Class: ErrorPermanent,
		Message: "duplicate record",
	}
)

// NewBridgeError creates a BridgeError with the given parameters.
func NewBridgeError(code ErrorCode, class ErrorClass, message string) *BridgeError {
	return &BridgeError{Code: code, Class: class, Message: message}
}

// AsBridgeError extracts a *BridgeError from an error chain.
func AsBridgeError(err error) (*BridgeError, bool) {
	var be *BridgeError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}

// IsRecoverableError returns true if the error is transient.
// Unknown error types are treated as recoverable (safe default).
func IsRecoverableError(err error) bool {
	if err == nil {
		return false
	}
	be, ok := AsBridgeError(err)
	if !ok {
		return true
	}
	return be.Class == ErrorTransient
}

// GetRetryAfter extracts the RetryAfter hint from an error chain.
func GetRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	be, ok := AsBridgeError(err)
	if !ok {
		return 0
	}
	return be.RetryAfter
}

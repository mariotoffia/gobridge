package servicebus

import (
	"context"
	"errors"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	bridgeTypes "github.com/mariotoffia/gobridge/bridge/types"
)

// MapError converts Azure Service Bus errors to BridgeError with correct classification.
func MapError(err error) *bridgeTypes.BridgeError {
	if err == nil {
		return nil
	}

	// Context errors
	if errors.Is(err, context.DeadlineExceeded) {
		return bridgeTypes.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return bridgeTypes.ErrUnavailable.Wrap(err)
	}

	// Check for Service Bus specific errors
	var sbErr *azservicebus.Error
	if errors.As(err, &sbErr) {
		return mapServiceBusError(sbErr)
	}

	// Check error message patterns
	errStr := err.Error()

	// Connection errors - recoverable
	if containsAny(errStr, "connection", "network", "timeout", "reset") {
		return bridgeTypes.ErrConnectionLost.Wrap(err)
	}

	// Throttling - recoverable
	if containsAny(errStr, "throttl", "busy", "overload", "too many") {
		return bridgeTypes.ErrThrottled.Wrap(err)
	}

	// Auth errors - permanent
	if containsAny(errStr, "unauthorized", "forbidden", "access denied", "401", "403") {
		return bridgeTypes.ErrNotAuthorized.Wrap(err)
	}

	// Not found - permanent
	if containsAny(errStr, "not found", "does not exist", "404") {
		return bridgeTypes.ErrNotFound.Wrap(err)
	}

	// Validation errors - permanent
	if containsAny(errStr, "invalid", "malformed", "bad request") {
		return bridgeTypes.ErrInvalidPayload.Wrap(err)
	}

	// Default: treat as recoverable (safe default)
	return bridgeTypes.ErrUnavailable.Wrap(err)
}

// mapServiceBusError maps azservicebus.Error to BridgeError.
func mapServiceBusError(sbErr *azservicebus.Error) *bridgeTypes.BridgeError {
	switch sbErr.Code {
	// Recoverable errors
	case azservicebus.CodeTimeout:
		return bridgeTypes.ErrTimeout.
			Wrap(sbErr).
			WithMessage("operation timed out")

	case azservicebus.CodeConnectionLost:
		return bridgeTypes.ErrConnectionLost.
			Wrap(sbErr).
			WithMessage("connection lost")

	case azservicebus.CodeLockLost:
		return bridgeTypes.ErrUnavailable.
			Wrap(sbErr).
			WithMessage("message lock lost - message may be redelivered")

	// Permanent errors
	case azservicebus.CodeUnauthorizedAccess:
		return bridgeTypes.ErrNotAuthorized.
			Wrap(sbErr).
			WithMessage("unauthorized access")

	default:
		// Unknown code - treat as recoverable
		return bridgeTypes.ErrUnavailable.
			Wrap(sbErr).
			With("code", string(sbErr.Code))
	}
}

// containsAny checks if s contains any of the substrings (case-insensitive).
func containsAny(s string, substrs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}


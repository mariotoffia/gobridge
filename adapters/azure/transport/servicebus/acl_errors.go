package servicebus

import (
	"context"
	"errors"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// MapError converts an Azure Service Bus error into a *shared.BridgeError
// with the appropriate ErrorClass for the runtime to decide retry vs DLQ.
func MapError(err error) *shared.BridgeError {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return shared.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return shared.ErrUnavailable.Wrap(err)
	}

	if errors.Is(err, azservicebus.ErrMessageTooLarge) {
		return shared.ErrPayloadTooLarge.Wrap(err).WithMessage("message too large")
	}

	var sbErr *azservicebus.Error
	if errors.As(err, &sbErr) {
		return mapServiceBusError(sbErr)
	}

	msg := err.Error()

	if containsAny(msg, "connection", "network", "reset") {
		return shared.ErrConnectionLost.Wrap(err)
	}
	if containsAny(msg, "throttl", "busy", "overload", "too many") {
		return shared.ErrThrottled.Wrap(err)
	}
	if containsAny(msg, "unauthorized", "forbidden", "access denied", "401", "403") {
		return shared.ErrNotAuthorized.Wrap(err)
	}
	if containsAny(msg, "not found", "does not exist", "404") {
		return shared.ErrNotFound.Wrap(err)
	}
	if containsAny(msg, "invalid", "malformed", "bad request") {
		return shared.ErrInvalidPayload.Wrap(err)
	}

	return shared.ErrUnavailable.Wrap(err)
}

func mapServiceBusError(sbErr *azservicebus.Error) *shared.BridgeError {
	switch sbErr.Code {
	case azservicebus.CodeTimeout:
		return shared.ErrTimeout.Wrap(sbErr).WithMessage("operation timed out")
	case azservicebus.CodeConnectionLost:
		return shared.ErrConnectionLost.Wrap(sbErr).WithMessage("connection lost")
	case azservicebus.CodeLockLost:
		return shared.ErrUnavailable.Wrap(sbErr).WithMessage("message lock lost - message may be redelivered")
	case azservicebus.CodeUnauthorizedAccess:
		return shared.ErrNotAuthorized.Wrap(sbErr).WithMessage("unauthorized access")
	default:
		return shared.ErrUnavailable.Wrap(sbErr).With("code", string(sbErr.Code))
	}
}

func containsAny(s string, substrs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

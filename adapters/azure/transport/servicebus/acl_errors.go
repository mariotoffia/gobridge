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
	case azservicebus.CodeNotFound:
		// The entity (queue/topic/subscription) does not exist — a
		// deleted entity or a wrong name (AMQP amqp:not-found, which the
		// SDK treats as RecoveryKindFatal). Classified PERMANENT
		// (ErrNotFound) so a settlement caller (Ack/Retry/Extend) sees a
		// non-recoverable class instead of the transient default.
		//
		// This classification does NOT self-fault the receive loop: the
		// poll loop retries EVERY receive error at the backoff cap
		// without consulting the error class (matching the SQS sibling by
		// design — an intentional cross-transport policy), and buildStack
		// re-wraps session-accept failures as transient ErrUnavailable. So
		// a deleted/renamed entity keeps polling at the backoff cap until
		// the entity reappears or the route is stopped; making it
		// self-fault would be a cross-transport behaviour change, out of
		// scope for this mapping.
		return shared.ErrNotFound.Wrap(sbErr).WithMessage("entity not found")
	case azservicebus.CodeClosed:
		// The local sender/receiver link or connection was closed
		// ("link was closed by user", or the ReceiveAndDelete internal
		// cache drained after Close). This is a LOCAL lifecycle
		// condition, not a remote entity-gone, and ASB clients/links are
		// rebuildable — so classify it TRANSIENT (ErrConnectionLost) and
		// let the poll loop rebuild rather than escalating a benign
		// shutdown/rotation race as a permanent fault. (Judgment call:
		// the finding leaned "likely permanent"; the SDK source shows
		// CodeClosed is a local link-close, not entity deletion, which
		// CodeNotFound covers.)
		return shared.ErrConnectionLost.Wrap(sbErr).WithMessage("link or connection closed")
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

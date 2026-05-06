package amqp10

import (
	"context"
	"errors"
	"strings"

	"github.com/Azure/go-amqp"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// MapError converts an AMQP 1.0 error into a *shared.BridgeError with the
// appropriate ErrorClass for the runtime to decide retry vs DLQ.
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

	var amqpErr *amqp.Error
	if errors.As(err, &amqpErr) {
		return mapAMQPCondition(amqpErr)
	}

	msg := err.Error()

	if containsAny(msg, "connection", "network", "reset", "broken pipe", "eof") {
		return shared.ErrConnectionLost.Wrap(err)
	}
	if containsAny(msg, "throttl", "busy", "overload", "too many") {
		return shared.ErrThrottled.Wrap(err)
	}
	if containsAny(msg, "unauthorized", "forbidden", "access denied") {
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

func mapAMQPCondition(amqpErr *amqp.Error) *shared.BridgeError {
	cond := string(amqpErr.Condition)

	switch cond {
	case "amqp:not-found":
		return shared.ErrNotFound.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:unauthorized-access":
		return shared.ErrNotAuthorized.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:not-allowed":
		return shared.ErrForbidden.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:resource-limit-exceeded":
		return shared.ErrThrottled.Wrap(amqpErr).WithMessage(amqpErr.Description)

	case "amqp:connection:forced":
		return shared.ErrConnectionLost.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:connection:framing-error":
		return shared.ErrProtocolError.Wrap(amqpErr).WithMessage(amqpErr.Description)

	case "amqp:session:errant-link":
		return shared.ErrUnavailable.Wrap(amqpErr).WithMessage(amqpErr.Description)

	case "amqp:link:detach-forced":
		return shared.ErrConnectionLost.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:link:transfer-limit-exceeded":
		return shared.ErrThrottled.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:link:message-size-exceeded":
		return shared.ErrPayloadTooLarge.Wrap(amqpErr).WithMessage(amqpErr.Description)

	case "amqp:internal-error":
		return shared.ErrUnavailable.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:not-implemented":
		return shared.ErrNotSupported.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:invalid-field":
		return shared.ErrInvalidPayload.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:decode-error":
		return shared.ErrProtocolError.Wrap(amqpErr).WithMessage(amqpErr.Description)
	}

	return shared.ErrUnavailable.Wrap(amqpErr).
		With("condition", cond).
		WithMessage(amqpErr.Description)
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

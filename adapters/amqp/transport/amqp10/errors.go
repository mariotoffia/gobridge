package amqp10

import (
	"context"
	"errors"
	"strings"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
)

// MapError converts an AMQP 1.0 error into a *domain.BridgeError with the
// appropriate ErrorClass for the runtime to decide retry vs DLQ.
func MapError(err error) *domain.BridgeError {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return domain.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return domain.ErrUnavailable.Wrap(err)
	}

	var amqpErr *amqp.Error
	if errors.As(err, &amqpErr) {
		return mapAMQPCondition(amqpErr)
	}

	msg := err.Error()

	if containsAny(msg, "connection", "network", "reset", "broken pipe", "eof") {
		return domain.ErrConnectionLost.Wrap(err)
	}
	if containsAny(msg, "throttl", "busy", "overload", "too many") {
		return domain.ErrThrottled.Wrap(err)
	}
	if containsAny(msg, "unauthorized", "forbidden", "access denied", "401", "403") {
		return domain.ErrNotAuthorized.Wrap(err)
	}
	if containsAny(msg, "not found", "does not exist", "404") {
		return domain.ErrNotFound.Wrap(err)
	}
	if containsAny(msg, "invalid", "malformed", "bad request") {
		return domain.ErrInvalidPayload.Wrap(err)
	}

	return domain.ErrUnavailable.Wrap(err)
}

func mapAMQPCondition(amqpErr *amqp.Error) *domain.BridgeError {
	cond := string(amqpErr.Condition)

	switch cond {
	case "amqp:not-found":
		return domain.ErrNotFound.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:unauthorized-access":
		return domain.ErrNotAuthorized.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:not-allowed":
		return domain.ErrForbidden.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:resource-limit-exceeded":
		return domain.ErrThrottled.Wrap(amqpErr).WithMessage(amqpErr.Description)

	case "amqp:connection:forced":
		return domain.ErrConnectionLost.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:connection:framing-error":
		return domain.ErrProtocolError.Wrap(amqpErr).WithMessage(amqpErr.Description)

	case "amqp:session:errant-link":
		return domain.ErrUnavailable.Wrap(amqpErr).WithMessage(amqpErr.Description)

	case "amqp:link:detach-forced":
		return domain.ErrConnectionLost.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:link:transfer-limit-exceeded":
		return domain.ErrThrottled.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:link:message-size-exceeded":
		return domain.ErrPayloadTooLarge.Wrap(amqpErr).WithMessage(amqpErr.Description)

	case "amqp:internal-error":
		return domain.ErrUnavailable.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:not-implemented":
		return domain.ErrNotSupported.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:invalid-field":
		return domain.ErrInvalidPayload.Wrap(amqpErr).WithMessage(amqpErr.Description)
	case "amqp:decode-error":
		return domain.ErrProtocolError.Wrap(amqpErr).WithMessage(amqpErr.Description)
	}

	return domain.ErrUnavailable.Wrap(amqpErr).
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

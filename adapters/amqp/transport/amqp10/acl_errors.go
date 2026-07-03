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
// Already-classified *shared.BridgeError values pass through unchanged.
func MapError(err error) *shared.BridgeError {
	if err == nil {
		return nil
	}

	var be *shared.BridgeError
	if errors.As(err, &be) {
		return be
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return shared.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return shared.ErrUnavailable.Wrap(err)
	}

	// Scope-typed SDK errors: map the peer-supplied condition when
	// present; otherwise classify by scope. A bare *amqp.LinkError with
	// nil RemoteErr (local detach, no condition) is transient — the link
	// can be re-attached on the live session.
	var linkErr *amqp.LinkError
	if errors.As(err, &linkErr) && linkErr.RemoteErr == nil {
		return shared.ErrUnavailable.Wrap(err).WithMessage("amqp10: link detached")
	}
	var connErr *amqp.ConnError
	if errors.As(err, &connErr) && connErr.RemoteErr == nil {
		return shared.ErrConnectionLost.Wrap(err)
	}
	var sessErr *amqp.SessionError
	if errors.As(err, &sessErr) && sessErr.RemoteErr == nil {
		return shared.ErrUnavailable.Wrap(err).WithMessage("amqp10: session ended")
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

// isLinkScopedError reports whether err is a fault confined to a single
// AMQP link (*amqp.LinkError) with the connection and session still
// healthy. Link-scoped faults must rebuild ONLY the link — escalating
// them to a full connection teardown (notifyDisconnect) would disrupt
// every other link on the session for a single-link problem.
func isLinkScopedError(err error) bool {
	var linkErr *amqp.LinkError
	if !errors.As(err, &linkErr) {
		return false
	}
	var connErr *amqp.ConnError
	var sessErr *amqp.SessionError
	return !errors.As(err, &connErr) && !errors.As(err, &sessErr)
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

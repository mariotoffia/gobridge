package amqp091

import (
	"context"
	"errors"
	"net"
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// MapError converts AMQP 0-9-1 / network errors into classified
// shared.BridgeError values for the bridge pipeline.
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
		return mapAMQPCode(amqpErr)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return shared.ErrTimeout.Wrap(err)
		}
		return shared.ErrConnectionLost.Wrap(err)
	}

	s := strings.ToLower(err.Error())
	if containsAny(s, "connection refused", "no route to host", "network unreachable", "connection reset") {
		return shared.ErrConnectionLost.Wrap(err)
	}
	if containsAny(s, "timeout", "timed out") {
		return shared.ErrTimeout.Wrap(err)
	}

	return shared.ErrUnavailable.Wrap(err)
}

func mapAMQPCode(e *amqp.Error) *shared.BridgeError {
	switch e.Code {
	case 320: // connection-forced
		return shared.ErrConnectionLost.Wrap(e)
	case 501, 502, 503, 505: // frame-error, syntax-error, command-invalid, unexpected-frame
		return shared.ErrProtocolError.Wrap(e)
	case 403: // access-refused
		return shared.ErrNotAuthorized.Wrap(e)
	case 404: // not-found
		return shared.ErrNotFound.Wrap(e)
	case 405, 530: // not-allowed
		return shared.ErrForbidden.Wrap(e)
	case 406, 540: // not-implemented
		return shared.ErrNotSupported.Wrap(e)
	case 504: // channel-error
		return shared.ErrUnavailable.Wrap(e)
	case 541: // internal-error
		return shared.ErrUnavailable.Wrap(e)
	default:
		return shared.ErrUnavailable.Wrap(e)
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

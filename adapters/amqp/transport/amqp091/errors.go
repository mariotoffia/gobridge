package amqp091

import (
	"context"
	"errors"
	"net"
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
)

// MapError converts AMQP 0-9-1 / network errors into classified
// domain.BridgeError values for the bridge pipeline.
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
		return mapAMQPCode(amqpErr)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return domain.ErrTimeout.Wrap(err)
		}
		return domain.ErrConnectionLost.Wrap(err)
	}

	s := strings.ToLower(err.Error())
	if containsAny(s, "connection refused", "no route to host", "network unreachable", "connection reset") {
		return domain.ErrConnectionLost.Wrap(err)
	}
	if containsAny(s, "timeout", "timed out") {
		return domain.ErrTimeout.Wrap(err)
	}

	return domain.ErrUnavailable.Wrap(err)
}

func mapAMQPCode(e *amqp.Error) *domain.BridgeError {
	switch e.Code {
	case 320: // connection-forced
		return domain.ErrConnectionLost.Wrap(e)
	case 501, 502, 503, 505: // frame-error, syntax-error, command-invalid, unexpected-frame
		return domain.ErrProtocolError.Wrap(e)
	case 403: // access-refused
		return domain.ErrNotAuthorized.Wrap(e)
	case 404: // not-found
		return domain.ErrNotFound.Wrap(e)
	case 405, 530: // not-allowed
		return domain.ErrForbidden.Wrap(e)
	case 406, 540: // not-implemented
		return domain.ErrNotSupported.Wrap(e)
	case 504: // channel-error
		return domain.ErrUnavailable.Wrap(e)
	case 541: // internal-error
		return domain.ErrUnavailable.Wrap(e)
	default:
		return domain.ErrUnavailable.Wrap(e)
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

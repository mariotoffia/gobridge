package paho

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/mariotoffia/gobridge/domain"
)

// MapError converts MQTT/paho errors to BridgeError with correct classification.
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

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return domain.ErrTimeout.Wrap(err)
		}
		return domain.ErrConnectionLost.Wrap(err)
	}

	s := err.Error()
	if containsAny(s, "connection refused", "no route to host", "network unreachable") {
		return domain.ErrConnectionLost.Wrap(err)
	}
	if containsAny(s, "server unavailable", "broker unavailable") {
		return domain.ErrUnavailable.Wrap(err)
	}

	return domain.ErrUnavailable.Wrap(err)
}

// MapDisconnectReasonCode converts an MQTT v5 DISCONNECT reason code to a
// BridgeError. Returns nil for normal disconnection (0x00).
func MapDisconnectReasonCode(code byte) *domain.BridgeError {
	switch code {
	case 0x00:
		return nil

	// Recoverable
	case 0x04:
		return domain.ErrUnavailable.Wrap(errors.New("disconnect with will message"))
	case 0x89:
		return domain.ErrBrokerBusy.Wrap(errors.New("server busy"))
	case 0x8D:
		return domain.ErrUnavailable.Wrap(errors.New("server shutting down"))
	case 0x8E:
		return domain.ErrTimeout.Wrap(errors.New("keep alive timeout"))
	case 0x8F:
		return domain.ErrConnectionLost.Wrap(errors.New("session taken over"))
	case 0x93:
		return domain.ErrThrottled.Wrap(errors.New("receive maximum exceeded"))
	case 0x97:
		return domain.ErrThrottled.Wrap(errors.New("quota exceeded"))
	case 0x9B:
		return domain.ErrQoSNotSupported.Wrap(errors.New("QoS not supported"))
	case 0x9D:
		return domain.ErrUnavailable.Wrap(errors.New("use another server"))
	case 0x9E:
		return domain.ErrUnavailable.Wrap(errors.New("server moved"))
	case 0xA1:
		return domain.ErrThrottled.Wrap(errors.New("connection rate exceeded"))

	// Permanent
	case 0x81:
		return domain.ErrProtocolError.Wrap(errors.New("malformed packet"))
	case 0x82:
		return domain.ErrProtocolError.Wrap(errors.New("protocol error"))
	case 0x83:
		return domain.ErrProtocolError.Wrap(errors.New("implementation specific error"))
	case 0x84:
		return domain.ErrProtocolError.Wrap(errors.New("unsupported protocol version"))
	case 0x85:
		return domain.ErrInvalidPayload.Wrap(errors.New("client identifier not valid"))
	case 0x86:
		return domain.ErrNotAuthorized.Wrap(errors.New("bad username or password"))
	case 0x87:
		return domain.ErrNotAuthorized.Wrap(errors.New("not authorized"))
	case 0x88:
		return domain.ErrUnavailable.Wrap(errors.New("server unavailable"))
	case 0x90:
		return domain.ErrInvalidTopic.Wrap(errors.New("topic filter invalid"))
	case 0x91:
		return domain.ErrInvalidTopic.Wrap(errors.New("topic name invalid"))
	case 0x94:
		return domain.ErrInvalidTopic.Wrap(errors.New("topic alias invalid"))
	case 0x95:
		return domain.ErrPayloadTooLarge.Wrap(errors.New("packet too large"))
	case 0x99:
		return domain.ErrInvalidPayload.Wrap(errors.New("payload format invalid"))
	case 0x9A:
		return domain.ErrProtocolError.Wrap(errors.New("retain not supported"))
	case 0x9C:
		return domain.ErrNotFound.Wrap(errors.New("no subscription existed"))
	case 0x9F:
		return domain.ErrProtocolError.Wrap(errors.New("shared subscriptions not supported"))
	case 0xA0:
		return domain.ErrProtocolError.Wrap(errors.New("subscription identifiers not supported"))
	case 0xA2:
		return domain.ErrProtocolError.Wrap(errors.New("wildcard subscriptions not supported"))

	default:
		return domain.ErrUnavailable.Wrap(fmt.Errorf("unknown disconnect reason code 0x%02X", code))
	}
}

// MapPublishReasonCode converts a PUBACK / PUBREC reason code to a
// BridgeError. Returns nil for success codes (0x00 and 0x10).
func MapPublishReasonCode(code byte) *domain.BridgeError {
	switch code {
	case 0x00: // Success
		return nil
	case 0x10: // No matching subscribers (still accepted by broker)
		return nil
	case 0x80:
		return domain.ErrUnavailable.Wrap(errors.New("unspecified publish error"))
	case 0x83:
		return domain.ErrProtocolError.Wrap(errors.New("implementation specific error"))
	case 0x87:
		return domain.ErrForbidden.Wrap(errors.New("not authorized to publish"))
	case 0x90:
		return domain.ErrInvalidTopic.Wrap(errors.New("topic name invalid"))
	case 0x91:
		return domain.ErrProtocolError.Wrap(errors.New("packet identifier in use"))
	case 0x97:
		return domain.ErrThrottled.Wrap(errors.New("quota exceeded"))
	case 0x99:
		return domain.ErrInvalidPayload.Wrap(errors.New("payload format invalid"))
	default:
		return domain.ErrUnavailable.Wrap(fmt.Errorf("unknown publish reason code 0x%02X", code))
	}
}

// MapSubscribeReasonCode converts a SUBACK reason code to a BridgeError.
// Returns nil for granted QoS codes (0x00, 0x01, 0x02).
func MapSubscribeReasonCode(code byte) *domain.BridgeError {
	switch code {
	case 0x00, 0x01, 0x02:
		return nil
	case 0x80:
		return domain.ErrUnavailable.Wrap(errors.New("unspecified subscribe error"))
	case 0x83:
		return domain.ErrProtocolError.Wrap(errors.New("implementation specific error"))
	case 0x87:
		return domain.ErrForbidden.Wrap(errors.New("not authorized to subscribe"))
	case 0x8F:
		return domain.ErrInvalidTopic.Wrap(errors.New("topic filter invalid"))
	case 0x91:
		return domain.ErrProtocolError.Wrap(errors.New("packet identifier in use"))
	case 0x97:
		return domain.ErrThrottled.Wrap(errors.New("quota exceeded"))
	case 0x9E:
		return domain.ErrProtocolError.Wrap(errors.New("shared subscriptions not supported"))
	case 0xA1:
		return domain.ErrProtocolError.Wrap(errors.New("subscription identifiers not supported"))
	case 0xA2:
		return domain.ErrProtocolError.Wrap(errors.New("wildcard subscriptions not supported"))
	default:
		return domain.ErrUnavailable.Wrap(fmt.Errorf("unknown subscribe reason code 0x%02X", code))
	}
}

func containsAny(s string, substrs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

package paho

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// MapError converts MQTT/paho errors to BridgeError with correct classification.
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

	// A server-side CONNECT denial arrives as a typed *autopaho.ConnackError
	// carrying the MQTT v5 reason code, so it classifies through the same
	// table the DISCONNECT path uses — including 0x88 "server unavailable",
	// which is the only classification the SDK's error strings ever carried.
	if code, ok := connackReasonCode(err); ok {
		if be := MapDisconnectReasonCode(code); be != nil {
			return be
		}
	}
	if pahoLinkDown(err) {
		return shared.ErrConnectionLost.Wrap(err)
	}
	if pahoInvalidArguments(err) {
		return shared.ErrProtocolError.Wrap(err)
	}

	// Dial and I/O failures (ECONNREFUSED, EHOSTUNREACH, ENETUNREACH, read
	// timeouts) reach us as *net.OpError, which satisfies net.Error.
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return shared.ErrTimeout.Wrap(err)
		}
		return shared.ErrConnectionLost.Wrap(err)
	}

	return shared.ErrUnavailable.Wrap(err)
}

// MapDisconnectReasonCode converts an MQTT v5 DISCONNECT reason code to a
// BridgeError. Returns nil for normal disconnection (0x00). The table
// follows MQTT v5 §3.14.2.1 (see also the constants in
// github.com/eclipse/paho.golang/packets/disconnect.go); CONNACK-only
// codes (0x84–0x86, 0x88) are also handled because autopaho surfaces
// both through the same disconnect path.
func MapDisconnectReasonCode(code byte) *shared.BridgeError {
	switch code {
	case 0x00:
		return nil

	// Recoverable
	case 0x04:
		return shared.ErrUnavailable.Wrap(errors.New("disconnect with will message"))
	case 0x89:
		return shared.ErrBrokerBusy.Wrap(errors.New("server busy"))
	case 0x8B:
		return shared.ErrUnavailable.Wrap(errors.New("server shutting down"))
	case 0x8D:
		return shared.ErrTimeout.Wrap(errors.New("keep alive timeout"))
	case 0x8E:
		return shared.ErrConnectionLost.Wrap(errors.New("session taken over"))
	case 0x93:
		return shared.ErrThrottled.Wrap(errors.New("receive maximum exceeded"))
	case 0x96:
		return shared.ErrThrottled.Wrap(errors.New("message rate too high"))
	case 0x97:
		return shared.ErrThrottled.Wrap(errors.New("quota exceeded"))
	case 0x9B:
		return shared.ErrQoSNotSupported.Wrap(errors.New("QoS not supported"))
	case 0x9C:
		return shared.ErrUnavailable.Wrap(errors.New("use another server"))
	case 0x9D:
		return shared.ErrUnavailable.Wrap(errors.New("server moved"))
	case 0x9F:
		return shared.ErrThrottled.Wrap(errors.New("connection rate exceeded"))
	case 0xA0:
		return shared.ErrUnavailable.Wrap(errors.New("maximum connect time"))

	// Permanent
	case 0x81:
		return shared.ErrProtocolError.Wrap(errors.New("malformed packet"))
	case 0x82:
		return shared.ErrProtocolError.Wrap(errors.New("protocol error"))
	case 0x83:
		return shared.ErrProtocolError.Wrap(errors.New("implementation specific error"))
	case 0x84:
		return shared.ErrProtocolError.Wrap(errors.New("unsupported protocol version"))
	case 0x85:
		return shared.ErrInvalidPayload.Wrap(errors.New("client identifier not valid"))
	case 0x86:
		return shared.ErrNotAuthorized.Wrap(errors.New("bad username or password"))
	case 0x87:
		return shared.ErrNotAuthorized.Wrap(errors.New("not authorized"))
	case 0x88:
		return shared.ErrUnavailable.Wrap(errors.New("server unavailable"))
	case 0x8F:
		return shared.ErrInvalidTopic.Wrap(errors.New("topic filter invalid"))
	case 0x90:
		return shared.ErrInvalidTopic.Wrap(errors.New("topic name invalid"))
	case 0x91:
		return shared.ErrProtocolError.Wrap(errors.New("packet identifier in use"))
	case 0x94:
		return shared.ErrInvalidTopic.Wrap(errors.New("topic alias invalid"))
	case 0x95:
		return shared.ErrPayloadTooLarge.Wrap(errors.New("packet too large"))
	case 0x98:
		return shared.ErrUnavailable.Wrap(errors.New("administrative action"))
	case 0x99:
		return shared.ErrInvalidPayload.Wrap(errors.New("payload format invalid"))
	case 0x9A:
		return shared.ErrProtocolError.Wrap(errors.New("retain not supported"))
	case 0x9E:
		return shared.ErrProtocolError.Wrap(errors.New("shared subscriptions not supported"))
	case 0xA1:
		return shared.ErrProtocolError.Wrap(errors.New("subscription identifiers not supported"))
	case 0xA2:
		return shared.ErrProtocolError.Wrap(errors.New("wildcard subscriptions not supported"))

	default:
		return shared.ErrUnavailable.Wrap(fmt.Errorf("unknown disconnect reason code 0x%02X", code))
	}
}

// MapPublishReasonCode converts a PUBACK / PUBREC reason code to a
// BridgeError. Returns nil for success codes (0x00 and 0x10).
func MapPublishReasonCode(code byte) *shared.BridgeError {
	switch code {
	case 0x00: // Success
		return nil
	case 0x10: // No matching subscribers (still accepted by broker)
		return nil
	case 0x80:
		return shared.ErrUnavailable.Wrap(errors.New("unspecified publish error"))
	case 0x83:
		return shared.ErrProtocolError.Wrap(errors.New("implementation specific error"))
	case 0x87:
		return shared.ErrForbidden.Wrap(errors.New("not authorized to publish"))
	case 0x90:
		return shared.ErrInvalidTopic.Wrap(errors.New("topic name invalid"))
	case 0x91:
		return shared.ErrProtocolError.Wrap(errors.New("packet identifier in use"))
	case 0x97:
		return shared.ErrThrottled.Wrap(errors.New("quota exceeded"))
	case 0x99:
		return shared.ErrInvalidPayload.Wrap(errors.New("payload format invalid"))
	default:
		return shared.ErrUnavailable.Wrap(fmt.Errorf("unknown publish reason code 0x%02X", code))
	}
}

// MapSubscribeReasonCode converts a SUBACK reason code to a BridgeError.
// Returns nil for granted QoS codes (0x00, 0x01, 0x02).
func MapSubscribeReasonCode(code byte) *shared.BridgeError {
	switch code {
	case 0x00, 0x01, 0x02:
		return nil
	case 0x80:
		return shared.ErrUnavailable.Wrap(errors.New("unspecified subscribe error"))
	case 0x83:
		return shared.ErrProtocolError.Wrap(errors.New("implementation specific error"))
	case 0x87:
		return shared.ErrForbidden.Wrap(errors.New("not authorized to subscribe"))
	case 0x8F:
		return shared.ErrInvalidTopic.Wrap(errors.New("topic filter invalid"))
	case 0x91:
		return shared.ErrProtocolError.Wrap(errors.New("packet identifier in use"))
	case 0x97:
		return shared.ErrThrottled.Wrap(errors.New("quota exceeded"))
	case 0x9E:
		return shared.ErrProtocolError.Wrap(errors.New("shared subscriptions not supported"))
	case 0xA1:
		return shared.ErrProtocolError.Wrap(errors.New("subscription identifiers not supported"))
	case 0xA2:
		return shared.ErrProtocolError.Wrap(errors.New("wildcard subscriptions not supported"))
	default:
		return shared.ErrUnavailable.Wrap(fmt.Errorf("unknown subscribe reason code 0x%02X", code))
	}
}

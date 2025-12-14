package mqtt

import (
	"context"
	"errors"
	"net"

	"github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// MapError converts MQTT/paho errors to BridgeError with correct classification.
// This ensures the pipeline can properly decide whether to retry or archive.
func MapError(err error) *types.BridgeError {
	if err == nil {
		return nil
	}

	// Context errors
	if errors.Is(err, context.DeadlineExceeded) {
		return types.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return types.ErrUnavailable.Wrap(err)
	}

	// Network errors - recoverable
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return types.ErrTimeout.Wrap(err)
		}
		return types.ErrConnectionLost.Wrap(err)
	}

	// Paho disconnect error (paho.Disconnect does not implement error, so we check by type assertion)
	// Note: paho.Disconnect is typically returned as a value, not wrapped in error chain

	// Check for common error patterns
	errStr := err.Error()

	// Connection errors
	if containsAny(errStr, "connection refused", "no route to host", "network unreachable") {
		return types.ErrConnectionLost.Wrap(err)
	}

	// Server errors
	if containsAny(errStr, "server unavailable", "broker unavailable") {
		return types.ErrUnavailable.Wrap(err)
	}

	// Default: treat as recoverable (safe default)
	return types.ErrUnavailable.Wrap(err)
}

// mapDisconnectError converts MQTT v5 disconnect reason codes to BridgeError.
func mapDisconnectError(d *paho.Disconnect) *types.BridgeError {
	switch d.ReasonCode {
	// Success - not really an error
	case 0x00: // Normal disconnection
		return nil

	// Recoverable errors
	case 0x04: // Disconnect with Will Message
		return types.ErrUnavailable.Wrap(errors.New("disconnect with will message"))
	case 0x89: // Server busy
		return types.ErrBrokerBusy.Wrap(errors.New("server busy"))
	case 0x8D: // Server shutting down
		return types.ErrUnavailable.Wrap(errors.New("server shutting down"))
	case 0x8E: // Keep alive timeout
		return types.ErrTimeout.Wrap(errors.New("keep alive timeout"))
	case 0x8F: // Session taken over
		return types.ErrConnectionLost.Wrap(errors.New("session taken over"))
	case 0x93: // Receive maximum exceeded
		return types.ErrThrottled.Wrap(errors.New("receive maximum exceeded"))
	case 0x97: // Quota exceeded
		return types.ErrThrottled.Wrap(errors.New("quota exceeded"))
	case 0x9B: // QoS not supported
		return types.ErrQoSNotSupported.Wrap(errors.New("QoS not supported"))
	case 0x9D: // Use another server
		return types.ErrUnavailable.Wrap(errors.New("use another server"))
	case 0x9E: // Server moved
		return types.ErrUnavailable.Wrap(errors.New("server moved"))
	case 0xA1: // Connection rate exceeded
		return types.ErrThrottled.Wrap(errors.New("connection rate exceeded"))

	// Permanent errors
	case 0x81: // Malformed packet
		return types.ErrProtocolError.Wrap(errors.New("malformed packet"))
	case 0x82: // Protocol error
		return types.ErrProtocolError.Wrap(errors.New("protocol error"))
	case 0x83: // Implementation specific error
		return types.ErrProtocolError.Wrap(errors.New("implementation specific error"))
	case 0x84: // Unsupported protocol version
		return types.ErrProtocolError.Wrap(errors.New("unsupported protocol version"))
	case 0x85: // Client identifier not valid
		return types.ErrInvalidPayload.Wrap(errors.New("client identifier not valid"))
	case 0x86: // Bad user name or password
		return types.ErrNotAuthorized.Wrap(errors.New("bad username or password"))
	case 0x87: // Not authorized
		return types.ErrNotAuthorized.Wrap(errors.New("not authorized"))
	case 0x88: // Server unavailable
		return types.ErrUnavailable.Wrap(errors.New("server unavailable"))
	case 0x90: // Topic filter invalid
		return types.ErrInvalidTopic.Wrap(errors.New("topic filter invalid"))
	case 0x91: // Topic name invalid
		return types.ErrInvalidTopic.Wrap(errors.New("topic name invalid"))
	case 0x94: // Topic alias invalid
		return types.ErrInvalidTopic.Wrap(errors.New("topic alias invalid"))
	case 0x95: // Packet too large
		return types.ErrPayloadTooLarge.Wrap(errors.New("packet too large"))
	case 0x99: // Payload format invalid
		return types.ErrInvalidPayload.Wrap(errors.New("payload format invalid"))
	case 0x9A: // Retain not supported
		return types.ErrProtocolError.Wrap(errors.New("retain not supported"))
	case 0x9C: // No subscription existed
		return types.ErrNotFound.Wrap(errors.New("no subscription existed"))
	case 0x9F: // Shared subscriptions not supported
		return types.ErrProtocolError.Wrap(errors.New("shared subscriptions not supported"))
	case 0xA0: // Subscription identifiers not supported
		return types.ErrProtocolError.Wrap(errors.New("subscription identifiers not supported"))
	case 0xA2: // Wildcard subscriptions not supported
		return types.ErrProtocolError.Wrap(errors.New("wildcard subscriptions not supported"))

	default:
		// Unknown reason code - treat as recoverable
		return types.ErrUnavailable.Wrap(errors.New("unknown disconnect reason"))
	}
}

// MapPublishError converts publish-specific errors to BridgeError.
func MapPublishError(err error, reasonCode byte) *types.BridgeError {
	if err == nil && reasonCode == 0 {
		return nil
	}

	// If we have a regular error, map it first
	if err != nil {
		return MapError(err)
	}

	// Map publish reason codes (PUBACK/PUBREC reason codes)
	switch reasonCode {
	case 0x00: // Success
		return nil
	case 0x10: // No matching subscribers
		// Not really an error - message was accepted by broker
		return nil
	case 0x80: // Unspecified error
		return types.ErrUnavailable.Wrap(errors.New("unspecified publish error"))
	case 0x83: // Implementation specific error
		return types.ErrProtocolError.Wrap(errors.New("implementation specific error"))
	case 0x87: // Not authorized
		return types.ErrForbidden.Wrap(errors.New("not authorized to publish"))
	case 0x90: // Topic name invalid
		return types.ErrInvalidTopic.Wrap(errors.New("topic name invalid"))
	case 0x91: // Packet identifier in use
		return types.ErrProtocolError.Wrap(errors.New("packet identifier in use"))
	case 0x97: // Quota exceeded
		return types.ErrThrottled.Wrap(errors.New("quota exceeded"))
	case 0x99: // Payload format invalid
		return types.ErrInvalidPayload.Wrap(errors.New("payload format invalid"))
	default:
		return types.ErrUnavailable.Wrap(errors.New("unknown publish error"))
	}
}

// containsAny checks if s contains any of the substrings.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}


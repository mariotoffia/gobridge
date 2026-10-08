package paho

import (
	"fmt"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// validateProtocol rejects the options the session's protocol version cannot
// express. MQTT 5 accepts every option. On MQTT 3.1.1 it rejects:
//
//   - no_local: 3.1.1 has no No-Local, and loop prevention must not silently
//     disappear (ADR 0010);
//   - a non-zero session_expiry_interval: 3.1.1 cannot send one, the broker
//     decides how long the session lives;
//   - a password without a username (MQTT-3.1.2-22);
//   - clean_start: true on a persistent session, which has no 3.1.1 wire form.
//
// mode is empty when the caller does not know the session mode yet; the
// mode-dependent rule then waits for ValidateEffectiveSession. Callers must pass
// options as configured, before NewSession's defaults are applied.
func (o SessionOptions) validateProtocol(mode connectivity.SessionMode) error {
	switch o.ProtocolVersion {
	case "", ProtocolVersion5:
		return nil
	case ProtocolVersion311:
	default:
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"mqtt: session.protocol_version must be %q or %q, got %q",
			ProtocolVersion5, ProtocolVersion311, o.ProtocolVersion))
	}
	if o.NoLocal {
		return errUnavailableOnMQTT311("session.no_local", "MQTT 3.1.1 has no No-Local; remove it, or use v5")
	}
	if o.SessionExpiryInterval != 0 {
		return errUnavailableOnMQTT311("session.session_expiry_interval",
			"MQTT 3.1.1 cannot send a session expiry, the broker decides how long the session lives; remove it, or use v5")
	}
	if err := credentialsExpressibleOnMQTT311(o.Username, !o.Password.IsZero()); err != nil {
		return err
	}
	if mode == connectivity.SessionPersistent && o.CleanStart {
		return errUnavailableOnMQTT311("session.clean_start",
			"clean_start: true on a persistent session has no MQTT 3.1.1 form; remove it, or use v5")
	}
	return nil
}

// credentialsExpressibleOnMQTT311 rejects a password without a username, which
// MQTT 3.1.1 forbids (MQTT-3.1.2-22). Live credential rotation calls it on the
// candidate before it changes anything.
func credentialsExpressibleOnMQTT311(username string, hasPassword bool) error {
	if hasPassword && username == "" {
		return errUnavailableOnMQTT311("session.password",
			"MQTT 3.1.1 forbids a password without a username; set session.username, or use v5")
	}
	return nil
}

func errUnavailableOnMQTT311(key, why string) error {
	return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
		"mqtt: %s is not available on session.protocol_version %s (%s)", key, ProtocolVersion311, why))
}

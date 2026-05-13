package amqp091

import (
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// wrapEnvelopeErr classifies an error returned by messaging.NewEnvelope
// into a *shared.BridgeError so callers can react with the bridge's
// error class taxonomy. ErrInvalidEnvelopeID maps to ErrCodeInvalidPayload
// (Rejected — the broker delivered an unusable message); the unexpected
// ErrEnvelopeClockMissing maps to ErrCodeInternal (Permanent) because
// the adapter always passes a non-zero clock and seeing it indicates a
// programmer error in this package, not a transient broker condition.
func wrapEnvelopeErr(err error) *shared.BridgeError {
	switch {
	case errors.Is(err, messaging.ErrInvalidEnvelopeID):
		return shared.ErrInvalidPayload.Wrap(fmt.Errorf("amqp091: %w", err))
	case errors.Is(err, messaging.ErrEnvelopeClockMissing):
		return shared.NewBridgeError(shared.ErrCodeInternal, shared.ErrorPermanent,
			"amqp091: envelope construction missing clock").Wrap(err)
	default:
		return shared.ErrInvalidPayload.Wrap(fmt.Errorf("amqp091: envelope construction: %w", err))
	}
}

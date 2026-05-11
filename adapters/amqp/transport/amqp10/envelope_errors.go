package amqp10

import (
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// wrapEnvelopeErr classifies a messaging.NewEnvelope failure into a
// *shared.BridgeError. ErrInvalidEnvelopeID is Rejected (the broker
// delivered an unusable message); ErrEnvelopeClockMissing is Internal
// because the adapter always passes a non-zero clock — seeing it
// indicates a programmer error in this package.
func wrapEnvelopeErr(err error) *shared.BridgeError {
	switch {
	case errors.Is(err, messaging.ErrInvalidEnvelopeID):
		return shared.ErrInvalidPayload.Wrap(fmt.Errorf("amqp10: %w", err))
	case errors.Is(err, messaging.ErrEnvelopeClockMissing):
		return shared.NewBridgeError(shared.ErrCodeInternal, shared.ErrorPermanent,
			"amqp10: envelope construction missing clock").Wrap(err)
	default:
		return shared.ErrInvalidPayload.Wrap(fmt.Errorf("amqp10: envelope construction: %w", err))
	}
}

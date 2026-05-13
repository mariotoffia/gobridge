package servicebus

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// generateEnvelopeID produces a 32-character hex envelope ID using
// crypto/rand. Used as the fallback when an Azure Service Bus message
// arrives with an empty MessageID; matches the format used by the
// AMQP091 / AMQP10 adapters so envelope IDs are uniformly shaped
// across origins. RNG failure is fatal because the platform RNG being
// unavailable is unrecoverable.
func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("servicebus: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// wrapEnvelopeErr classifies a messaging.NewEnvelope failure into a
// *shared.BridgeError. ErrInvalidEnvelopeID maps to ErrCodeInvalidPayload
// (Rejected — broker delivered an unusable message); the unexpected
// ErrEnvelopeClockMissing maps to ErrCodeInternal (Permanent) because
// the adapter always passes a non-zero clock.
func wrapEnvelopeErr(err error) *shared.BridgeError {
	switch {
	case errors.Is(err, messaging.ErrInvalidEnvelopeID):
		return shared.ErrInvalidPayload.Wrap(fmt.Errorf("servicebus: %w", err))
	case errors.Is(err, messaging.ErrEnvelopeClockMissing):
		return shared.NewBridgeError(shared.ErrCodeInternal, shared.ErrorPermanent,
			"servicebus: envelope construction missing clock").Wrap(err)
	default:
		return shared.ErrInvalidPayload.Wrap(fmt.Errorf("servicebus: envelope construction: %w", err))
	}
}

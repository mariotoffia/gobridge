package messaging_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// An envelope ID is the identity every durable record is keyed by, and every
// one of those records is written through Envelope.MarshalJSON. encoding/json
// replaces a byte sequence that is not valid UTF-8 with U+FFFD, so an ID that
// is not valid UTF-8 comes back from the DLQ or the outbox as a DIFFERENT
// identity than the one that went in: a redrive then injects a message the
// replay ledger cannot match to the original, and the accounting that makes
// at-least-once delivery countable silently breaks.
//
// The bridge therefore refuses such an ID where it is created rather than
// corrupting it where it is stored. Adapters already normalise: the MQTT
// adapter base64-encodes binary Correlation Data before using it as an
// identity (ADR-0001 — reserved-header trust model covers the header side of
// the same rule).
//
// The rehydration path needs no separate rejection: a JSON string cannot
// express invalid UTF-8 at all — the decoder substitutes U+FFFD — so a stored
// record can only ever produce a valid-UTF-8 identity. That substitution IS the
// corruption this guard exists to prevent, which is why it sits at
// construction.
//
// Category: unit (TESTS.md §1).

func TestNewEnvelope_RejectsAnIDThatCannotSurviveThePersistedForm(t *testing.T) {
	_, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:      "\xb9",
		Subject: "orders",
	}, time.Unix(1, 0))

	require.ErrorIs(t, err, messaging.ErrInvalidEnvelopeID,
		"an ID that is not valid UTF-8 loses its identity on the first store write")
}

func TestNewEnvelope_AcceptsANonASCIIIDThatIsValidUTF8(t *testing.T) {
	envelope, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:      "ordrar-åäö-🌍",
		Subject: "orders",
	}, time.Unix(1, 0))
	require.NoError(t, err)

	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)

	var decoded messaging.Envelope
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, envelope.ID(), decoded.ID())
}

func TestAssignID_RejectsAnIDThatCannotSurviveThePersistedForm(t *testing.T) {
	// AssignID's guard is reachable only on an envelope that has no identity
	// yet, which construction never produces — a zero value does.
	var envelope messaging.Envelope
	require.ErrorIs(t, envelope.AssignID("\xff"), messaging.ErrInvalidEnvelopeID,
		"AssignID must apply the same identity rule as construction")
	require.NoError(t, envelope.AssignID("ok"))
}

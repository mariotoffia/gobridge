package messaging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnvelope_Payload_InputSliceMutationDoesNotLeak verifies that
// mutating the slice passed to EnvelopeInput.Payload after construction
// does NOT affect the envelope's stored payload (constructor defensive-copy).
func TestEnvelope_Payload_InputSliceMutationDoesNotLeak(t *testing.T) {
	payload := []byte("hello")
	e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "p1", Payload: payload})
	payload[0] = 'X'
	assert.Equal(t, "hello", string(e.Payload()))
}

// TestEnvelope_Payload_AccessorReturnsCopy verifies that the slice
// returned by Payload() is an independent copy (mutating the returned
// slice does not alter the envelope state) and that an empty payload
// returns nil.
func TestEnvelope_Payload_AccessorReturnsCopy(t *testing.T) {
	e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "p2", Payload: []byte("hello")})
	got := e.Payload()
	got[0] = 'X'
	assert.Equal(t, "hello", string(e.Payload()), "accessor must return independent copy")

	// empty payload → nil
	empty := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "p2b"})
	assert.Nil(t, empty.Payload(), "empty payload must be nil")
}

// TestEnvelope_SetPayload_DefensiveCopy verifies that SetPayload copies
// the input so a subsequent mutation of the caller's slice does not bleed
// into the envelope.
func TestEnvelope_SetPayload_DefensiveCopy(t *testing.T) {
	e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "p3"})
	in := []byte("abc")
	e.SetPayload(in)
	in[0] = 'Z'
	assert.Equal(t, "abc", string(e.Payload()))
}

// TestEnvelope_SetExpiry exercises the three SetExpiry branches:
//
//	(a) expiry before CreatedAt → ErrExpiryBeforeCreated, state unchanged
//	(b) valid future expiry → nil, ExpiresAt updated
//	(c) zero time → nil, expiry cleared
func TestEnvelope_SetExpiry(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	e := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        "s1",
		CreatedAt: t0,
	})

	// (a) expiry before created
	err := e.SetExpiry(t0.Add(-time.Hour))
	require.True(t, errors.Is(err, messaging.ErrExpiryBeforeCreated))
	assert.True(t, e.ExpiresAt().IsZero(), "ExpiresAt must remain zero after rejected expiry")

	// (b) valid future expiry
	future := t0.Add(time.Hour)
	require.NoError(t, e.SetExpiry(future))
	assert.True(t, e.ExpiresAt().Equal(future))

	// (c) zero clears expiry
	require.NoError(t, e.SetExpiry(time.Time{}))
	assert.True(t, e.ExpiresAt().IsZero(), "zero expiry must clear ExpiresAt")
}

// TestEnvelope_AssignID covers all three AssignID branches. An ID-less
// envelope is obtained the same way production does: UnmarshalJSON of a
// payload missing the "ID" key yields an envelope whose id is blank (the
// rehydration path that bridge_routes.go's synthetic-injection path then
// assigns an ID to). MustEnvelope auto-assigns a unique ID and NewEnvelope
// rejects an empty ID, so neither constructor can produce one.
func TestEnvelope_AssignID(t *testing.T) {
	// (a) blank ID → success. Rehydrate an envelope with no "ID" key.
	var blank messaging.Envelope
	require.NoError(t, json.Unmarshal([]byte(`{"Subject":"s"}`), &blank))
	require.Equal(t, "", blank.ID())
	require.NoError(t, blank.AssignID("assigned"))
	assert.Equal(t, "assigned", blank.ID())

	// (b) already-set ID → ErrEnvelopeIDImmutable (identity is immutable).
	e := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "existing"})
	err := e.AssignID("y")
	require.True(t, errors.Is(err, messaging.ErrEnvelopeIDImmutable))
	assert.Equal(t, "existing", e.ID())

	// (c) empty arg → ErrInvalidEnvelopeID.
	err = e.AssignID("")
	require.True(t, errors.Is(err, messaging.ErrInvalidEnvelopeID))
}

// TestEnvelope_JSON_RoundTripStableSchema guards the durable wire format:
// the JSON keys must use the historical capitalised names, and a
// marshal→unmarshal round-trip must preserve every field.
func TestEnvelope_JSON_RoundTripStableSchema(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	e := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        "j1",
		Subject:   "s",
		Payload:   []byte("body"),
		CreatedAt: t0,
		ExpiresAt: t0.Add(time.Hour),
	})

	raw, err := json.Marshal(e)
	require.NoError(t, err)

	// golden wire-format guard — key names must be stable
	for _, key := range []string{`"ID"`, `"Payload"`, `"CreatedAt"`, `"ExpiresAt"`, `"Subject"`} {
		assert.True(t, bytes.Contains(raw, []byte(key)), "JSON must contain key %s", key)
	}

	// round-trip
	var e2 messaging.Envelope
	require.NoError(t, json.Unmarshal(raw, &e2))
	assert.Equal(t, e.ID(), e2.ID())
	assert.Equal(t, string(e.Payload()), string(e2.Payload()))
	assert.Equal(t, e.Subject(), e2.Subject())
	assert.True(t, e.CreatedAt().Equal(e2.CreatedAt()))
	assert.True(t, e.ExpiresAt().Equal(e2.ExpiresAt()))
}

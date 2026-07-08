package messaging_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestUnmarshalJSON_RejectsEmptyID pins finding 4: the rehydration path
// enforces the SAME non-empty-ID invariant NewEnvelope does, so a corrupt or
// hand-forged record cannot rehydrate into an identity-less envelope that would
// collide in the DLQ / outbox key space. Both an absent "ID" key and an
// explicit empty "ID" are rejected with ErrInvalidEnvelopeID.
func TestUnmarshalJSON_RejectsEmptyID(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"absent id key", `{"Subject":"s","Payload":"aGk="}`},
		{"explicit empty id", `{"ID":"","Subject":"s"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e messaging.Envelope
			err := json.Unmarshal([]byte(tc.data), &e)
			if err == nil {
				t.Fatalf("expected UnmarshalJSON to reject empty ID, got nil")
			}
			if !errors.Is(err, messaging.ErrInvalidEnvelopeID) {
				t.Fatalf("want ErrInvalidEnvelopeID, got %v", err)
			}
		})
	}
}

// TestUnmarshalJSON_ValidRoundTripSucceeds proves the empty-ID guard does not
// regress the normal durable round-trip: a fully-formed envelope marshals and
// unmarshals back with identity, subject, payload and timestamps intact.
func TestUnmarshalJSON_ValidRoundTripSucceeds(t *testing.T) {
	created := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	expires := created.Add(time.Hour)
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        "env-1",
		Subject:   "orders.created",
		Payload:   []byte(`{"amount":10}`),
		CreatedAt: created,
		ExpiresAt: expires,
	})

	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got messaging.Envelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal valid envelope: %v", err)
	}
	if got.ID() != "env-1" {
		t.Fatalf("ID = %q, want %q", got.ID(), "env-1")
	}
	if got.Subject() != "orders.created" {
		t.Fatalf("Subject = %q, want %q", got.Subject(), "orders.created")
	}
	if string(got.Payload()) != `{"amount":10}` {
		t.Fatalf("Payload = %q, want %q", string(got.Payload()), `{"amount":10}`)
	}
	if !got.CreatedAt().Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt(), created)
	}
	if !got.ExpiresAt().Equal(expires) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt(), expires)
	}
}

// TestUnmarshalJSON_NumericHeaderRehydratesAsFloat64 documents the deliberate
// (and now godoc'd) type drift: encoding/json decodes every JSON number as
// float64, so a header written as a typed int comes back as float64 after any
// DLQ / outbox round-trip. This test ASSERTS the documented behaviour so it is
// a pinned contract, not an accident — callers must coerce, never bare-int
// assert, after a round-trip. The reserved bridge headers are strings and are
// unaffected.
func TestUnmarshalJSON_NumericHeaderRehydratesAsFloat64(t *testing.T) {
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-drift",
		Subject: "s",
		Headers: map[string]any{
			"retry-attempt": int(7),
			"ratio":         float64(1.5),
			"label":         "keep-as-string",
		},
	})

	// Precondition: before any round-trip the int header is still a Go int.
	if v, _ := orig.Header("retry-attempt"); v != int(7) {
		t.Fatalf("precondition: header should be int(7), got %T(%v)", v, v)
	}

	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got messaging.Envelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The int header drifted to float64 (documented behaviour).
	v, ok := got.Header("retry-attempt")
	if !ok {
		t.Fatal("retry-attempt header missing after round-trip")
	}
	f, isFloat := v.(float64)
	if !isFloat {
		t.Fatalf("retry-attempt should rehydrate as float64, got %T", v)
	}
	if f != 7 {
		t.Fatalf("retry-attempt value = %v, want 7", f)
	}
	if _, stillInt := v.(int); stillInt {
		t.Fatal("retry-attempt must NOT remain a Go int after a JSON round-trip")
	}

	// A float header is unchanged (already float64), a string stays a string.
	if v, _ := got.Header("ratio"); v != float64(1.5) {
		t.Fatalf("ratio = %T(%v), want float64(1.5)", v, v)
	}
	if v, _ := got.Header("label"); v != "keep-as-string" {
		t.Fatalf("label = %T(%v), want string", v, v)
	}
}

package messaging_test

import (
	"encoding/json"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Every durable record this bridge writes — DLQ entry, outbox row, committed
// config artifact — goes out through Envelope.MarshalJSON and comes back
// through UnmarshalJSON. Seed cases prove the shapes someone thought of; only
// mutation reaches the ones nobody did: a header key that is a lone surrogate,
// a payload that is not valid UTF-8, a subject longer than any real address, a
// reserved-prefix key differing only in case.
//
// Two invariants must survive arbitrary input, because a store that breaks
// either loses or corrupts a message that was already accepted:
//
//   - Identity and body round-trip byte for byte. The ID is refused at
//     construction unless it can (see envelope_id_roundtrip_test.go), and the
//     payload travels as base64, so both are lossless for any input.
//   - The reserved namespace stays closed at construction and open on
//     rehydration — NewEnvelope strips an `x-bridge.` key a producer sent,
//     while a bridge-stamped one survives a save and load. See
//     ADR-0001 — reserved-header trust model.
//
// Free text — the subject and header strings — is NORMALISED rather than
// preserved: a JSON string cannot express invalid UTF-8, so the encoder
// substitutes U+FFFD. That is a deliberate boundary, not a defect, because a
// Subject is a logical NAME and a name is text; the fields the runtime keys on
// are the ones that must be exact. The check below therefore requires text that
// is already valid UTF-8 to survive unchanged, which is the guarantee callers
// actually rely on.
//
// Category: unit (TESTS.md §1). Run mutation with `make fuzz`.

func FuzzEnvelopeHeaderRoundTrip(f *testing.F) {
	f.Add("id-1", "orders/eu", []byte("body"), "tenant", "acme")
	f.Add("id-2", "", []byte(nil), messaging.HeaderPrefix+"route-id", "r1")
	f.Add("id-3", "a/b/c", []byte{0x00, 0xff, 0xfe}, "X-BRIDGE.Correlation-Id", "c1")
	f.Add("åäö", "s", []byte("\xed\xa0\x80"), "k", "\xed\xa0\x80")
	f.Add("id-5", "s", []byte("p"), "", "")

	f.Fuzz(func(t *testing.T, id, subject string, payload []byte, headerKey, headerValue string) {
		if id == "" {
			t.Skip("an empty ID is rejected at construction; UnmarshalJSON pins that separately")
		}
		envelope, err := messaging.NewEnvelope(messaging.EnvelopeInput{
			ID:      id,
			Subject: subject,
			Payload: payload,
			Headers: map[string]any{headerKey: headerValue},
		}, time.Unix(1, 0))
		if err != nil {
			// Construction rejects some inputs (an ID that is not valid UTF-8,
			// for instance). Rejection is a correct answer; a panic is not.
			return
		}

		if messaging.IsReservedHeader(headerKey) {
			if _, present := envelope.Header(headerKey); present {
				t.Fatalf("producer-supplied reserved header %q survived construction", headerKey)
			}
		}

		encoded, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshal %q: %v", id, err)
		}
		if !utf8.Valid(encoded) {
			t.Fatalf("marshalled form of %q is not valid UTF-8", id)
		}

		var decoded messaging.Envelope
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal what we just marshalled (%q): %v", id, err)
		}

		if decoded.ID() != envelope.ID() {
			t.Fatalf("ID %q round-tripped to %q", envelope.ID(), decoded.ID())
		}
		if utf8.ValidString(subject) && decoded.Subject() != envelope.Subject() {
			t.Fatalf("subject %q round-tripped to %q", envelope.Subject(), decoded.Subject())
		}
		if string(decoded.Payload()) != string(envelope.Payload()) {
			t.Fatalf("payload of %q changed across the round trip", id)
		}
		if decoded.Headers().Len() != envelope.Headers().Len() {
			t.Fatalf("header count of %q changed: %d -> %d",
				id, envelope.Headers().Len(), decoded.Headers().Len())
		}
		if utf8.ValidString(headerKey) && utf8.ValidString(headerValue) {
			for key, value := range envelope.Headers().Snapshot() {
				got, present := decoded.Header(key)
				if !present {
					t.Fatalf("header %q did not survive the round trip", key)
				}
				if got != value {
					t.Fatalf("header %q round-tripped from %v to %v", key, value, got)
				}
			}
		}
	})
}

// FuzzEnvelopeRehydrationRejectsCorruptRecords pins the other direction: a
// hand-forged or corrupted stored record must fail cleanly. A record that
// rehydrates into an identity-less envelope would collide in the DLQ and outbox
// key space, so an empty ID is the one construction invariant this path keeps.
func FuzzEnvelopeRehydrationRejectsCorruptRecords(f *testing.F) {
	f.Add(`{"ID":"a","Subject":"s"}`)
	f.Add(`{"ID":""}`)
	f.Add(`{"Payload":"!!!not-base64"}`)
	f.Add(`{"CreatedAt":"not-a-time"}`)
	f.Add(`{`)
	f.Add(`{"ID":"a","Headers":{"x-bridge.route-id":"r"}}`)

	f.Fuzz(func(t *testing.T, record string) {
		var decoded messaging.Envelope
		if err := json.Unmarshal([]byte(record), &decoded); err != nil {
			return
		}
		if decoded.ID() == "" {
			t.Fatalf("record %q rehydrated into an envelope with no identity", record)
		}
		// A record that decoded must re-encode; a store that reads a row it
		// cannot write back cannot redrive it.
		if _, err := json.Marshal(&decoded); err != nil {
			t.Fatalf("record %q decoded but will not re-encode: %v", record, err)
		}
	})
}

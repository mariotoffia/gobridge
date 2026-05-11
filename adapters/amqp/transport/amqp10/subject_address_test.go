// Tests covering the subject/address separation contract for the
// AMQP 1.0 adapter (T07):
//
//   - Sender links are address-bound; non-empty OutboundMessage.Address
//     must match the configured sender link address. Mismatches are
//     rejected with shared.ErrInvalidTopic without contacting the broker.
//   - Empty Address means "use the configured link address".
//   - The logical Envelope.Subject travels via amqp.Properties.Subject
//     and never selects the link target.
//   - The receiver does NOT fall back to the configured link address
//     when an inbound Properties.Subject is missing — Envelope.Subject
//     is left empty in that case.
package amqp10

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// recordingSenderLink captures every envelope passed to SendEnvelope so
// tests can assert that no link send happened (mismatched-address path)
// or that the recorded envelope preserves Subject independently of the
// link's bound address.
type recordingSenderLink struct {
	mu         sync.Mutex
	sent       []*messaging.Envelope
	sendErr    error
	closeErr   error
	closeCalls int
}

func (r *recordingSenderLink) SendEnvelope(_ context.Context, env *messaging.Envelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, env)
	return r.sendErr
}

func (r *recordingSenderLink) Close(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeCalls++
	return r.closeErr
}

func (r *recordingSenderLink) sentCopy() []*messaging.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*messaging.Envelope, len(r.sent))
	copy(out, r.sent)
	return out
}

// newSenderWithLink wires a Sender with a pre-installed link so Send()
// skips link creation and exercises only the validation/dispatch path
// under test.
func newSenderWithLink(t *testing.T, address string, link senderLinkAPI) *Sender {
	t.Helper()
	sess := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		connectivity.SessionEphemeral, nil)
	s, err := NewSender(SenderConfig{Address: address, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	s.mu.Lock()
	s.link = link
	s.mu.Unlock()
	return s
}

// TestSender_Send_RejectsMismatchedAddress verifies that a non-empty
// OutboundMessage.Address that differs from the configured link
// address is rejected with shared.ErrInvalidTopic and that no link
// send is attempted.
func TestSender_Send_RejectsMismatchedAddress(t *testing.T) {
	link := &recordingSenderLink{}
	s := newSenderWithLink(t, "queue/configured", link)

	err := s.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte("p")}),
		Address:  "queue/other",
	})
	if err == nil {
		t.Fatal("Send() must fail when Address mismatches the configured link address")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidTopic) {
		t.Fatalf("err = %v, want ErrInvalidTopic", err)
	}
	if got := link.sentCopy(); len(got) != 0 {
		t.Fatalf("link saw %d sends, want 0 (mismatch must be rejected before dispatch)", len(got))
	}
}

// TestSender_Send_AcceptsMatchingAddress verifies that a non-empty
// Address equal to the configured link address succeeds and the
// envelope is forwarded unchanged.
func TestSender_Send_AcceptsMatchingAddress(t *testing.T) {
	link := &recordingSenderLink{}
	s := newSenderWithLink(t, "queue/configured", link)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte("p")})
	err := s.Send(context.Background(), ports.OutboundMessage{
		Envelope: env,
		Address:  "queue/configured",
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	got := link.sentCopy()
	if len(got) != 1 {
		t.Fatalf("link saw %d sends, want 1", len(got))
	}
	if got[0].Subject() != "evt.x" {
		t.Fatalf("Subject = %q, want %q (subject must survive the dispatch)", got[0].Subject(), "evt.x")
	}
}

// TestSender_Send_DefaultsToConfiguredAddressOnEmpty verifies that an
// empty OutboundMessage.Address is permitted (means "use the configured
// link address") and the send succeeds.
func TestSender_Send_DefaultsToConfiguredAddressOnEmpty(t *testing.T) {
	link := &recordingSenderLink{}
	s := newSenderWithLink(t, "queue/configured", link)

	err := s.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte("p")}),
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := link.sentCopy(); len(got) != 1 {
		t.Fatalf("link saw %d sends, want 1", len(got))
	}
}

// TestSender_Send_PreservesEnvelopeSubject pins that envelopeToMessage
// maps Envelope.Subject to Properties.Subject regardless of how the
// dispatch was addressed (matching vs. empty Address).
func TestSender_Send_PreservesEnvelopeSubject(t *testing.T) {
	cases := []struct {
		name    string
		address string
	}{
		{"empty-address", ""},
		{"matching-address", "queue/configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link := &recordingSenderLink{}
			s := newSenderWithLink(t, "queue/configured", link)

			err := s.Send(context.Background(), ports.OutboundMessage{
				Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-x", Subject: "logical.subject", Payload: []byte("p")}),
				Address:  tc.address,
			})
			if err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			got := link.sentCopy()
			if len(got) != 1 || got[0].Subject() != "logical.subject" {
				t.Fatalf("captured envelopes = %+v, want one with Subject=%q", got, "logical.subject")
			}
			// Confirm Subject crosses the SDK seam intact.
			msg := envelopeToMessage(got[0])
			if msg.Properties == nil || msg.Properties.Subject == nil || *msg.Properties.Subject != "logical.subject" {
				t.Fatalf("Properties.Subject = %v, want %q", msg.Properties, "logical.subject")
			}
		})
	}
}

// TestSender_Send_NilEnvelopeReturnsInvalidPayload verifies that
// Send rejects a nil envelope with shared.ErrInvalidPayload before
// touching the link.
func TestSender_Send_NilEnvelopeReturnsInvalidPayload(t *testing.T) {
	link := &recordingSenderLink{}
	s := newSenderWithLink(t, "queue/configured", link)

	err := s.Send(context.Background(), ports.OutboundMessage{Envelope: nil})
	if err == nil {
		t.Fatal("Send() must fail on nil envelope")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
	if got := link.sentCopy(); len(got) != 0 {
		t.Fatalf("link saw %d sends, want 0", len(got))
	}
}

// TestSender_SendBatch_AddressValidation verifies SendBatch fails fast
// (sent=0, ErrInvalidTopic) when any item's Address mismatches the
// configured link address, and succeeds when every item is empty/matching.
func TestSender_SendBatch_AddressValidation(t *testing.T) {
	t.Run("mismatched-first-item-fails-fast", func(t *testing.T) {
		link := &recordingSenderLink{}
		s := newSenderWithLink(t, "queue/configured", link)

		sent, err := s.SendBatch(context.Background(), []ports.OutboundMessage{
			{Envelope: &messaging.Envelope{ID: "a"}, Address: "queue/wrong"},
			{Envelope: &messaging.Envelope{ID: "b"}},
		})
		if sent != 0 {
			t.Fatalf("sent = %d, want 0", sent)
		}
		var be *shared.BridgeError
		if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidTopic) {
			t.Fatalf("err = %v, want ErrInvalidTopic", err)
		}
		if got := link.sentCopy(); len(got) != 0 {
			t.Fatalf("link saw %d sends, want 0", len(got))
		}
	})

	t.Run("all-matching-succeeds-and-subjects-propagate", func(t *testing.T) {
		link := &recordingSenderLink{}
		s := newSenderWithLink(t, "queue/configured", link)

		sent, err := s.SendBatch(context.Background(), []ports.OutboundMessage{
			{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "a", Subject: "s.a"}), Address: "queue/configured"},
			{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b", Subject: "s.b"})}, // empty Address is allowed
		})
		if err != nil {
			t.Fatalf("SendBatch() error = %v", err)
		}
		if sent != 2 {
			t.Fatalf("sent = %d, want 2", sent)
		}
		got := link.sentCopy()
		if len(got) != 2 {
			t.Fatalf("link saw %d sends, want 2", len(got))
		}
		if got[0].Subject() != "s.a" || got[1].Subject() != "s.b" {
			t.Fatalf("captured subjects = [%q, %q], want [%q, %q]",
				got[0].Subject(), got[1].Subject(), "s.a", "s.b")
		}
	})
}

// TestSender_SendBatch_NilEnvelopeFailsFast verifies SendBatch
// surfaces shared.ErrInvalidPayload with sent=0 when any entry
// contains a nil envelope.
func TestSender_SendBatch_NilEnvelopeFailsFast(t *testing.T) {
	link := &recordingSenderLink{}
	s := newSenderWithLink(t, "queue/configured", link)

	sent, err := s.SendBatch(context.Background(), []ports.OutboundMessage{
		{Envelope: nil},
	})
	if sent != 0 {
		t.Fatalf("sent = %d, want 0", sent)
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
}

// TestMessageToEnvelope_NoFallbackOnMissingSubject pins the T07
// acceptance criterion: an inbound AMQP 1.0 message without
// Properties.Subject MUST yield Envelope.Subject == "" (no link-address
// fallback).
func TestMessageToEnvelope_NoFallbackOnMissingSubject(t *testing.T) {
	msg := &amqp.Message{Data: [][]byte{[]byte("body")}}

	env, err := messageToEnvelope(msg, clock.System)
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	if env.Subject() != "" {
		t.Fatalf("Subject = %q, want empty (acceptance: no Properties.Subject ⇒ empty Envelope.Subject)",
			env.Subject())
	}
}

// TestMessageToEnvelope_PreservesProvidedSubject verifies that an
// inbound Properties.Subject ends up in Envelope.Subject verbatim.
func TestMessageToEnvelope_PreservesProvidedSubject(t *testing.T) {
	subj := "logical.subject"
	msg := &amqp.Message{
		Properties: &amqp.MessageProperties{Subject: &subj},
		Data:       [][]byte{[]byte("body")},
	}

	env, err := messageToEnvelope(msg, clock.System)
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	if env.Subject() != subj {
		t.Fatalf("Subject = %q, want %q", env.Subject(), subj)
	}
	// The raw amqp10.subject header continues to be recorded under
	// Headers via messageToHeaders for full property round-trip.
	if env.Headers()[headerSubject] != subj {
		t.Fatalf("Headers[%s] = %v, want %q", headerSubject, env.Headers()[headerSubject], subj)
	}
}

// TestRoundTrip_SubjectIndependentOfAddress proves Subject survives an
// outbound (envelopeToMessage) → inbound (messageToEnvelope) cycle
// without leaking the configured link address into Subject.
func TestRoundTrip_SubjectIndependentOfAddress(t *testing.T) {
	out := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "rt-1",
		Subject: "logical.subject",
		Payload: []byte("body"),
	})

	msg := envelopeToMessage(out)
	in, err := messageToEnvelope(msg, clock.System)
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	if in.Subject() != "logical.subject" {
		t.Fatalf("round-trip Subject = %q, want %q", in.Subject(), "logical.subject")
	}
}

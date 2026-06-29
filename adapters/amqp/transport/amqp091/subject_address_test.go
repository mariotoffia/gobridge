// Tests covering the subject/address separation contract for the
// AMQP 0-9-1 adapter (T06):
//
//   - Sender uses OutboundMessage.Address first, then SenderConfig.RoutingKey.
//   - Sender publishes the logical Envelope.Subject as a HeaderGobridgeSubject
//     entry in the AMQP Headers table — never as the routing key.
//   - Receiver populates Envelope.Subject ONLY from HeaderGobridgeSubject
//     and never from the inbound RoutingKey.
//   - The transport routing key is recorded under HeaderRoutingKey on receive.
//   - An inbound HeaderGobridgeSubject from a peer bridge is honoured and
//     does NOT leak into the generic envelope-headers pass-through.
package amqp091

import (
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// verifies resolveRoutingKey prefers msg.Address over cfg.RoutingKey
// (per-dispatch destination wins over adapter default).
func TestResolveRoutingKey_AddressWinsOverConfig(t *testing.T) {
	cfg := SenderConfig{RoutingKey: "rk-cfg"}
	msg := ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e", Subject: "logical.subject"}),
		Address:  "rk-addr",
	}

	got, err := resolveRoutingKey(cfg, msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "rk-addr" {
		t.Errorf("routingKey = %q, want %q (Address must win over cfg)", got, "rk-addr")
	}
}

// verifies resolveRoutingKey falls back to cfg.RoutingKey when Address is empty.
func TestResolveRoutingKey_FallsBackToConfig(t *testing.T) {
	cfg := SenderConfig{RoutingKey: "rk-cfg"}
	msg := ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e", Subject: "logical.subject"}),
	}

	got, err := resolveRoutingKey(cfg, msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "rk-cfg" {
		t.Errorf("routingKey = %q, want %q", got, "rk-cfg")
	}
}

// verifies resolveRoutingKey ignores Envelope.Subject entirely — the
// logical subject is no longer a routing-key fallback.
func TestResolveRoutingKey_SubjectIsNotConsulted(t *testing.T) {
	cfg := SenderConfig{}
	msg := ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e", Subject: "logical.subject"}),
	}

	_, err := resolveRoutingKey(cfg, msg)
	if err == nil {
		t.Fatal("expected ErrInvalidTopic when Address and cfg.RoutingKey both empty")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidTopic) {
		t.Fatalf("expected ErrInvalidTopic, got %v", err)
	}
}

// verifies envelopeToPublishing emits the logical Subject as a typed
// HeaderGobridgeSubject entry in the AMQP Headers table.
func TestEnvelopeToPublishing_EmitsGobridgeSubjectHeader(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-subj",
		Subject: "logical.subject",
		Payload: []byte("body"),
	})

	pub := envelopeToPublishing(env, SenderConfig{}, nil)

	if pub.Headers == nil {
		t.Fatal("Headers table is nil; expected gobridge.subject entry")
	}
	got, ok := pub.Headers[HeaderGobridgeSubject].(string)
	if !ok {
		t.Fatalf("Headers[%s] missing or wrong type: %v (%T)",
			HeaderGobridgeSubject,
			pub.Headers[HeaderGobridgeSubject],
			pub.Headers[HeaderGobridgeSubject])
	}
	if got != "logical.subject" {
		t.Errorf("Headers[%s] = %q, want %q",
			HeaderGobridgeSubject, got, "logical.subject")
	}
}

// verifies envelopeToPublishing does NOT emit gobridge.subject when
// env.Subject() is empty and no peer-bridge round-trip header was supplied.
func TestEnvelopeToPublishing_OmitsSubjectWhenEmpty(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-no-subj",
		Payload: []byte("body"),
	})

	pub := envelopeToPublishing(env, SenderConfig{}, nil)

	if pub.Headers != nil {
		if _, ok := pub.Headers[HeaderGobridgeSubject]; ok {
			t.Errorf("Headers[%s] should be absent when Subject is empty",
				HeaderGobridgeSubject)
		}
	}
}

// verifies envelopeToPublishing preserves a peer-supplied gobridge.subject
// header when Envelope.Subject itself is empty (subject round-trip from
// another transport).
func TestEnvelopeToPublishing_PreservesPeerSuppliedSubjectHeader(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-peer",
		Payload: []byte("body"),
		Headers: map[string]any{
			HeaderGobridgeSubject: "peer.subject",
		},
	})

	pub := envelopeToPublishing(env, SenderConfig{}, nil)

	if pub.Headers == nil {
		t.Fatal("Headers table is nil; peer-supplied gobridge.subject must be preserved")
	}
	got, _ := pub.Headers[HeaderGobridgeSubject].(string)
	if got != "peer.subject" {
		t.Errorf("Headers[%s] = %q, want %q (peer round-trip)",
			HeaderGobridgeSubject, got, "peer.subject")
	}
}

// verifies envelopeToPublishing prefers env.Subject() over a stale
// gobridge.subject header when both are present.
func TestEnvelopeToPublishing_SubjectWinsOverStaleHeader(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-both",
		Subject: "current.subject",
		Payload: []byte("body"),
		Headers: map[string]any{
			HeaderGobridgeSubject: "stale.subject",
		},
	})

	pub := envelopeToPublishing(env, SenderConfig{}, nil)

	got, _ := pub.Headers[HeaderGobridgeSubject].(string)
	if got != "current.subject" {
		t.Errorf("Headers[%s] = %q, want %q (env.Subject() must win)",
			HeaderGobridgeSubject, got, "current.subject")
	}
}

// verifies deliveryToEnvelope sources Envelope.Subject from the
// HeaderGobridgeSubject AMQP user header and records the inbound
// transport routing key under HeaderRoutingKey.
func TestDeliveryToEnvelope_SubjectFromGobridgeHeader(t *testing.T) {
	d := amqp.Delivery{
		MessageId:  "msg-700",
		RoutingKey: "rk-x",
		Body:       []byte("payload"),
		Headers: amqp.Table{
			HeaderGobridgeSubject: "logical.subject",
		},
	}

	env, err := deliveryToEnvelope(d, nil)
	if err != nil {
		t.Fatalf("deliveryToEnvelope: %v", err)
	}

	if env.Subject() != "logical.subject" {
		t.Errorf("Subject = %q, want %q (must come only from gobridge.subject)",
			env.Subject(), "logical.subject")
	}
	if env.Headers()[HeaderRoutingKey] != "rk-x" {
		t.Errorf("Headers[%s] = %v, want %q",
			HeaderRoutingKey, env.Headers()[HeaderRoutingKey], "rk-x")
	}
}

// verifies deliveryToEnvelope leaves Envelope.Subject empty when the
// inbound delivery carries no HeaderGobridgeSubject — the routing key
// is NEVER promoted to Subject.
func TestDeliveryToEnvelope_NoSubjectHeaderLeavesSubjectEmpty(t *testing.T) {
	d := amqp.Delivery{
		MessageId:  "msg-701",
		RoutingKey: "rk-y",
		Body:       []byte("payload"),
	}

	env, err := deliveryToEnvelope(d, nil)
	if err != nil {
		t.Fatalf("deliveryToEnvelope: %v", err)
	}

	if env.Subject() != "" {
		t.Errorf("Subject = %q, want empty (no gobridge.subject header present)",
			env.Subject())
	}
	if env.Headers()[HeaderRoutingKey] != "rk-y" {
		t.Errorf("Headers[%s] = %v, want %q",
			HeaderRoutingKey, env.Headers()[HeaderRoutingKey], "rk-y")
	}
}

// verifies deliveryToHeaders strips HeaderGobridgeSubject from the
// generic pass-through map: the typed extraction in deliveryToEnvelope
// owns it, and it must not be duplicated under env.Headers().
func TestDeliveryToHeaders_StripsGobridgeSubject(t *testing.T) {
	d := amqp.Delivery{
		Headers: amqp.Table{
			HeaderGobridgeSubject: "logical.subject",
			"safe-key":            "keep-me",
		},
	}

	h := deliveryToHeaders(d)

	if _, ok := h[HeaderGobridgeSubject]; ok {
		t.Errorf("deliveryToHeaders must not leak %s into the generic header map",
			HeaderGobridgeSubject)
	}
	if h["safe-key"] != "keep-me" {
		t.Errorf("safe-key dropped; got %v", h["safe-key"])
	}
}

// verifies headersToPublishing skips HeaderGobridgeSubject in its
// generic header pass-through (envelopeToPublishing owns the typed write).
func TestHeadersToPublishing_SkipsGobridgeSubject(t *testing.T) {
	headers := map[string]any{
		HeaderGobridgeSubject: "ignored.here",
		"tenant":              "acme",
	}

	pub := headersToPublishing(headers)

	if pub.Headers == nil {
		t.Fatal("Headers table is nil")
	}
	if _, ok := pub.Headers[HeaderGobridgeSubject]; ok {
		t.Errorf("headersToPublishing must skip %s; envelopeToPublishing owns it",
			HeaderGobridgeSubject)
	}
	if pub.Headers["tenant"] != "acme" {
		t.Errorf("tenant dropped; got %v", pub.Headers["tenant"])
	}
}

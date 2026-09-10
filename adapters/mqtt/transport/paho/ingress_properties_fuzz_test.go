package paho_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// MQTT v5 lets a producer attach arbitrary binary Correlation Data and an
// arbitrary number of User Properties with arbitrary keys and values. Seed
// cases cover the shapes someone thought of; only mutation reaches the ones
// nobody did — a key that is a lone surrogate, a value with an embedded NUL, a
// property named exactly like a bridge-reserved header, Correlation Data one
// byte over the retention bound.
//
// Four invariants must hold for every combination, because a producer on the
// wire is untrusted input:
//
//   - The reserved namespace stays closed. A wire User Property in the
//     `x-bridge.` namespace never becomes a trusted bridge header — that is
//     ADR-0001 — reserved-header trust model, and spoofing it is how a
//     producer would forge a route override or a correlation identity.
//   - Every publish yields a usable identity, and two deliveries of the same
//     publish yield the SAME one. An unstable identity makes a redelivery
//     uncountable, so the replay cap terminalises the first transient failure
//     instead of retrying.
//   - A header the adapter accepts is within the bound it advertises
//     (`docs/transports/mqtt-options.md`); an oversized or unsafe one is
//     dropped and counted, never truncated into something else.
//   - The egress hop reproduces the body it was handed and never leaks an
//     internal-only header back onto the wire.
//
// Category: unit (TESTS.md §1). Run mutation with `make fuzz`.

// ingressHeaderBound is the adapter's advertised User Property length bound.
// It is duplicated here on purpose: a test that read the constant would follow
// the code down whatever the code did, and the bound is a published contract.
const ingressHeaderBound = 256

func FuzzIngressPublishProperties(f *testing.F) {
	f.Add("sensors/1", []byte("body"), []byte("corr-1"), "text/plain", "tenant", "acme")
	f.Add("a/b", []byte(nil), []byte{0x00, 0xff}, "", "x-bridge.route-id", "spoof")
	f.Add("t", []byte("p"), []byte(nil), strings.Repeat("c", 300), "k", strings.Repeat("v", 300))
	f.Add("t", []byte("p"), []byte("\xed\xa0\x80"), "ct", "\xed\xa0\x80", "\xed\xa0\x80")
	f.Add("", []byte("p"), []byte("c"), "ct", "", "")

	f.Fuzz(func(t *testing.T, topic string, payload, correlation []byte,
		contentType, propertyKey, propertyValue string,
	) {
		newPublish := func() *pahov5.Publish {
			return &pahov5.Publish{
				Topic:   topic,
				Payload: append([]byte(nil), payload...),
				Properties: &pahov5.PublishProperties{
					CorrelationData: append([]byte(nil), correlation...),
					ContentType:     contentType,
					User: pahov5.UserProperties{
						{Key: propertyKey, Value: propertyValue},
						// A forged route override: the adapter derives no route
						// at ingress, so this value must never appear.
						{Key: messaging.HeaderRouteID, Value: "forged-" + propertyValue},
					},
				},
			}
		}

		envelope := paho.EnvelopeFromPublish(newPublish(), nil)
		if envelope == nil {
			t.Fatalf("topic %q produced no envelope", topic)
		}

		if forged, present := envelope.Header(messaging.HeaderRouteID); present {
			t.Fatalf("a wire User Property forged the reserved header %s = %v",
				messaging.HeaderRouteID, forged)
		}

		if envelope.ID() == "" {
			t.Fatalf("topic %q produced an envelope with no identity", topic)
		}
		if !utf8.ValidString(envelope.ID()) {
			t.Fatalf("identity %q is not valid UTF-8, so it cannot be persisted", envelope.ID())
		}
		if second := paho.EnvelopeFromPublish(newPublish(), nil); second.ID() != envelope.ID() {
			// Only a producer-supplied identity is required to be stable; an
			// adapter-minted one is unique per delivery and says so.
			if _, minted := envelope.Header(messaging.HeaderGeneratedID); !minted {
				t.Fatalf("producer identity is unstable across redelivery: %q then %q",
					envelope.ID(), second.ID())
			}
		}

		for key, value := range envelope.HeadersSnapshot() {
			text, isText := value.(string)
			if !isText || messaging.IsReservedHeader(key) || key == paho.HeaderMQTTTopic {
				// Reserved keys are adapter-derived, and the transport topic is
				// bounded upstream by the ingress packet guard, not by the User
				// Property rule under test here.
				continue
			}
			if len(key) > ingressHeaderBound || len(text) > ingressHeaderBound {
				t.Fatalf("header %q=%q exceeds the advertised %d-byte bound",
					key, text, ingressHeaderBound)
			}
			if !utf8.ValidString(key) || !utf8.ValidString(text) {
				t.Fatalf("header %q=%q is not valid UTF-8 and must have been dropped", key, text)
			}
		}

		egress, err := paho.PublishFromEnvelope(envelope, "out/"+topic, paho.SenderOptions{}, nil)
		if err != nil {
			return
		}
		if string(egress.Payload) != string(payload) {
			t.Fatalf("payload changed across the ingress/egress round trip for topic %q", topic)
		}
		for _, property := range egress.Properties.User {
			if messaging.IsInternalOnlyHeader(property.Key) {
				t.Fatalf("internal-only header %q leaked onto the wire", property.Key)
			}
		}
	})
}

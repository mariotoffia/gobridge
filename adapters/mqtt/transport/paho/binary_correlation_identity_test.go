package paho

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// binaryCorrelation is MQTT v5 Correlation Data that is legal on the wire
// (Correlation Data is a binary property) but is not a safe header string: it
// carries a NUL byte and a byte sequence that is not valid UTF-8.
var binaryCorrelation = []byte{0x00, 0x01, 0xff, 0xfe, 'a', 'b'}

// TestCorrelationIdentity_BinaryDataIsStableAcrossRedelivery proves a producer
// that identifies its message ONLY through binary Correlation Data keeps one
// stable envelope identity across a broker redelivery.
//
// A redelivered publish carries the same Correlation Data bytes, so the derived
// identity must be byte-identical and must NOT be marked adapter-generated —
// otherwise the runtime treats every redelivery as a fresh, uncountable
// identity and terminalizes the first transient failure instead of retrying.
func TestCorrelationIdentity_BinaryDataIsStableAcrossRedelivery(t *testing.T) {
	newDelivery := func() *pahov5.Publish {
		return &pahov5.Publish{
			Topic:   "sensors/1",
			Payload: []byte("p"),
			Properties: &pahov5.PublishProperties{
				CorrelationData: append([]byte(nil), binaryCorrelation...),
			},
		}
	}

	first := EnvelopeFromPublish(publishWithIdentity(newDelivery()), nil)
	second := EnvelopeFromPublish(publishWithIdentity(newDelivery()), nil)

	if first.ID() == "" {
		t.Fatal("binary correlation data must yield a non-empty envelope id")
	}
	if first.ID() != second.ID() {
		t.Fatalf("redelivery identity is unstable: first=%q second=%q", first.ID(), second.ID())
	}
	if _, generated := messaging.GetHeaderString(first.Headers(), messaging.HeaderGeneratedID); generated {
		t.Fatal("binary correlation data is a stable producer identity; it must not be marked adapter-generated")
	}
}

// TestCorrelationIdentity_BinaryDataRoundTripsUnchanged proves the raw
// Correlation Data bytes survive the envelope hop: an ingress publish carrying
// non-UTF-8 correlation data produces an egress publish with byte-identical
// Correlation Data, and the encoded retention header never leaks as a user
// property.
func TestCorrelationIdentity_BinaryDataRoundTripsUnchanged(t *testing.T) {
	env := EnvelopeFromPublish(&pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: append([]byte(nil), binaryCorrelation...),
		},
	}, nil)

	encoded, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationData)
	if !ok {
		t.Fatalf("headers[%q] missing; raw correlation data was discarded", messaging.HeaderCorrelationData)
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(encoded); err != nil || !bytes.Equal(decoded, binaryCorrelation) {
		t.Fatalf("headers[%q] = %q does not decode to the original bytes (err=%v)", messaging.HeaderCorrelationData, encoded, err)
	}
	if _, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationID); ok {
		t.Fatalf("binary correlation data must not be published as the string correlation id header")
	}

	out := mustPublishFromEnvelope(t, env, "out/topic", SenderOptions{}, nil)
	if out.Properties == nil {
		t.Fatal("egress publish carries no properties; correlation data was lost")
	}
	if !bytes.Equal(out.Properties.CorrelationData, binaryCorrelation) {
		t.Fatalf("egress CorrelationData = %v, want %v", out.Properties.CorrelationData, binaryCorrelation)
	}
	for _, u := range out.Properties.User {
		if u.Key == messaging.HeaderCorrelationData {
			t.Fatalf("adapter-controlled key %q leaked as an egress user property", messaging.HeaderCorrelationData)
		}
	}
}

// TestCorrelationIdentity_BinaryDataSurvivesTheRuntimeCorrelationStamp proves
// the round trip holds in the shape egress actually sees.
//
// The runtime stamps x-bridge.correlation-id on every envelope that arrives
// without one, so a binary-correlation message reaches egress carrying BOTH
// headers. If the textual value won there, every such message would leave with a
// random bridge id in place of the producer's bytes — the retention header would
// be dead code and the round trip would exist only in tests that bypass the
// runtime.
func TestCorrelationIdentity_BinaryDataSurvivesTheRuntimeCorrelationStamp(t *testing.T) {
	env := EnvelopeFromPublish(&pahov5.Publish{
		Topic:      "t",
		Properties: &pahov5.PublishProperties{CorrelationData: append([]byte(nil), binaryCorrelation...)},
	}, nil)
	// What RouteRunner.injectHeaders does before dispatch.
	env.SetHeader(messaging.HeaderCorrelationID, "bridge-generated-correlation")

	out := mustPublishFromEnvelope(t, env, "out/topic", SenderOptions{}, nil)
	if out.Properties == nil {
		t.Fatal("egress publish carries no properties; correlation data was lost")
	}
	if !bytes.Equal(out.Properties.CorrelationData, binaryCorrelation) {
		t.Fatalf("egress CorrelationData = %v, want the producer's bytes %v",
			out.Properties.CorrelationData, binaryCorrelation)
	}
}

// TestCorrelationIdentity_InboundBinaryHeaderCannotBeSpoofed proves the
// retention header is producer-proof, on this transport and on any other.
//
// Only an ingress adapter that actually read binary correlation off the wire may
// install it. A publisher sending a user property of that name is stopped by the
// reserved-prefix filter here; a producer on a different source transport is
// stopped by NewEnvelope's reserved strip, which every adapter's ingress routes
// its external headers through. Were the key unreserved, an SQS or AMQP producer
// could dictate — and displace the bridge's own correlation id in — what an MQTT
// egress hop puts on the wire.
func TestCorrelationIdentity_InboundBinaryHeaderCannotBeSpoofed(t *testing.T) {
	spoofed := base64.RawURLEncoding.EncodeToString([]byte("spoofed"))

	t.Run("as an inbound MQTT user property", func(t *testing.T) {
		env := EnvelopeFromPublish(&pahov5.Publish{
			Topic: "t",
			Properties: &pahov5.PublishProperties{
				User: []pahov5.UserProperty{{Key: messaging.HeaderCorrelationData, Value: spoofed}},
			},
		}, nil)

		if v, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationData); ok {
			t.Fatalf("publisher-supplied %q survived ingress with value %q", messaging.HeaderCorrelationData, v)
		}
	})

	t.Run("as an application header from another source transport", func(t *testing.T) {
		// NewEnvelope is the construction path every source adapter routes its
		// external headers through; it strips the reserved namespace.
		env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
			ID:      "from-another-transport",
			Payload: []byte("p"),
			Headers: map[string]any{
				messaging.HeaderCorrelationData: spoofed,
				"application-key":               "kept",
			},
		}, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("NewEnvelope: %v", err)
		}
		if v, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationData); ok {
			t.Fatalf("producer-supplied %q survived ingress with value %q", messaging.HeaderCorrelationData, v)
		}
		env.SetHeader(messaging.HeaderCorrelationID, "bridge-correlation")

		out := mustPublishFromEnvelope(t, env, "out/topic", SenderOptions{}, nil)
		if out.Properties == nil {
			t.Fatal("egress publish carries no properties")
		}
		if string(out.Properties.CorrelationData) != "bridge-correlation" {
			t.Fatalf("egress CorrelationData = %q, want the bridge's own correlation id",
				out.Properties.CorrelationData)
		}
	})
}

// TestCorrelationIdentity_OverLongBinaryDataIsCountedAndUnused proves oversized
// binary Correlation Data is neither retained nor used as identity: it is
// counted on the ingress header-drop metric and the publish falls back to an
// adapter-minted identity.
func TestCorrelationIdentity_OverLongBinaryDataIsCountedAndUnused(t *testing.T) {
	oversized := bytes.Repeat([]byte{0x00}, maxHeaderValueLen+1)
	exporter := &ports.RecordingExporter{}

	env := EnvelopeFromPublish(publishWithIdentity(&pahov5.Publish{
		Topic:      "t",
		Properties: &pahov5.PublishProperties{CorrelationData: oversized},
	}), nil, exporter)

	if _, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationData); ok {
		t.Fatalf("oversized correlation data must not be retained as a header")
	}
	if _, generated := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID); !generated {
		t.Fatal("an unusable correlation identity must fall back to an adapter-minted, generated identity")
	}
	if len(exporter.FindEntries(MetricMQTTIngressHeaderDropped)) == 0 {
		t.Fatalf("oversized correlation data drop is not counted on %s", MetricMQTTIngressHeaderDropped)
	}
}

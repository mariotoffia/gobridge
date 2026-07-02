package paho

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// TestPublishFromEnvelope_NonStringHeaderIncrementsCounter is the MQTT-N1
// regression: a bridge-to-bridge header whose value is NOT a string (here a
// non-string idempotency-key) is dropped on egress because it cannot become
// an MQTT user property. Before the fix the drop was silent; now it must
// increment MetricMQTTNonStringHeaderDropped so the loss is observable. A
// well-formed string header alongside it must still round-trip untouched.
func TestPublishFromEnvelope_NonStringHeaderIncrementsCounter(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "id-1",
		Payload: []byte("body"),
	})
	// StampHeaders is the trusted whole-map setter (no coercion), so the
	// int value survives as a non-string on the envelope.
	env.StampHeaders(map[string]any{
		messaging.HeaderIdempotencyKey: 12345,     // non-string -> dropped + counted
		"x-app-note":                   "keep-me", // string -> passes through
	})

	rec := &ports.RecordingExporter{}
	pub := PublishFromEnvelope(env, "t/out", SenderOptions{QoS: 1}, nil, rec)

	entries := rec.FindEntries(MetricMQTTNonStringHeaderDropped)
	if len(entries) != 1 {
		t.Fatalf("MetricMQTTNonStringHeaderDropped entries = %d, want 1", len(entries))
	}
	if entries[0].IValue != 1 {
		t.Fatalf("dropped count = %d, want 1", entries[0].IValue)
	}

	// Sanity: the non-string idempotency-key must NOT appear as a user
	// property, while the string application header must.
	var sawIdem, sawNote bool
	if pub.Properties != nil {
		for _, u := range pub.Properties.User {
			switch u.Key {
			case messaging.HeaderIdempotencyKey:
				sawIdem = true
			case "x-app-note":
				sawNote = true
			}
		}
	}
	if sawIdem {
		t.Error("non-string idempotency-key must not be serialised as a user property")
	}
	if !sawNote {
		t.Error("string application header must still pass through as a user property")
	}
}

// TestPublishFromEnvelope_AllStringHeaders_NoDropCounter confirms the
// counter is NOT emitted when every propagated header value is a string —
// the drop signal must be specific to the non-string case.
func TestPublishFromEnvelope_AllStringHeaders_NoDropCounter(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "id-2",
		Payload: []byte("body"),
	})
	env.StampHeaders(map[string]any{
		messaging.HeaderIdempotencyKey: "idem-abc",
		"x-app-note":                   "keep-me",
	})

	rec := &ports.RecordingExporter{}
	_ = PublishFromEnvelope(env, "t/out", SenderOptions{QoS: 1}, nil, rec)

	if entries := rec.FindEntries(MetricMQTTNonStringHeaderDropped); len(entries) != 0 {
		t.Fatalf("MetricMQTTNonStringHeaderDropped entries = %d, want 0", len(entries))
	}
}

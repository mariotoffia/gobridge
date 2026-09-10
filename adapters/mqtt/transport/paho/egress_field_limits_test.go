package paho

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// mqttFieldLimitOverflow is one byte past the largest value an MQTT v5
// length-prefixed field can carry.
const mqttFieldLimitOverflow = mqttStringFieldLimit + 1

// TestPublishFromEnvelope_RejectsOverlongWireFields pins that no outbound field
// is ever handed to the SDK truncated. Paho's writeString/writeBinary slice any
// value longer than 65,535 bytes and write the shortened form with no error, so
// the broker would acknowledge metadata that differs from the source: a cut
// idempotency key stops deduplicating, a cut tenant id mis-attributes, a cut
// correlation id breaks the reply path, and a cut UTF-8 value can be invalid
// UTF-8 on the wire. Construction must fail instead, with a stable permanent
// classification so the route treats it as a rejection rather than retrying.
func TestPublishFromEnvelope_RejectsOverlongWireFields(t *testing.T) {
	oversized := strings.Repeat("x", mqttFieldLimitOverflow)

	cases := map[string]struct {
		topic   string
		id      string
		subject string
		headers map[string]any
	}{
		"topic": {
			topic: oversized,
			id:    "id-1",
		},
		"message_id_user_property": {
			topic: "t/out",
			id:    oversized,
		},
		"subject_user_property": {
			topic:   "t/out",
			id:      "id-1",
			subject: oversized,
		},
		"application_header_value": {
			topic:   "t/out",
			id:      "id-1",
			headers: map[string]any{"x-app-note": oversized},
		},
		"application_header_key": {
			topic:   "t/out",
			id:      "id-1",
			headers: map[string]any{oversized: "value"},
		},
		"content_type": {
			topic:   "t/out",
			id:      "id-1",
			headers: map[string]any{messaging.HeaderContentType: oversized},
		},
		"response_topic": {
			topic:   "t/out",
			id:      "id-1",
			headers: map[string]any{headerMQTTResponseTopic: oversized},
		},
		"textual_correlation_id": {
			topic:   "t/out",
			id:      "id-1",
			headers: map[string]any{messaging.HeaderCorrelationID: oversized},
		},
		"binary_correlation_data": {
			topic: "t/out",
			id:    "id-1",
			headers: map[string]any{
				messaging.HeaderCorrelationData: base64.RawURLEncoding.EncodeToString(
					make([]byte, mqttFieldLimitOverflow)),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      tc.id,
				Subject: tc.subject,
				Payload: []byte("body"),
			})
			if tc.headers != nil {
				env.StampHeaders(tc.headers)
			}

			pub, err := PublishFromEnvelope(env, tc.topic, SenderOptions{QoS: 1}, nil)

			require.Error(t, err, "an over-long field must fail construction, never truncate")
			require.ErrorIs(t, err, shared.ErrPayloadTooLarge)
			require.Equal(t, shared.ErrorRejected, err.(*shared.BridgeError).Class)
			require.Nil(t, pub, "a rejected publish must not be handed to the caller")
		})
	}
}

// TestPublishFromEnvelope_RejectsMalformedUTF8Fields pins the other way a wire
// field can be illegal. MQTT v5 §1.5.4 requires well-formed UTF-8 and forbids
// U+0000; Paho encodes whatever bytes it is handed, so a header carrying
// invalid UTF-8 leaves as a malformed packet and the broker answers with a
// DISCONNECT — recycling the session for every message that reproduces it.
func TestPublishFromEnvelope_RejectsMalformedUTF8Fields(t *testing.T) {
	cases := map[string]struct {
		topic   string
		headers map[string]any
	}{
		"invalid_utf8_header_value": {
			topic:   "t/out",
			headers: map[string]any{"x-app-note": "\xff\xfe"},
		},
		"invalid_utf8_header_key": {
			topic:   "t/out",
			headers: map[string]any{"x-\xff": "value"},
		},
		"nul_in_header_value": {
			topic:   "t/out",
			headers: map[string]any{"x-app-note": "a\x00b"},
		},
		"invalid_utf8_topic": {
			topic: "t/\xff",
		},
		"invalid_utf8_content_type": {
			topic:   "t/out",
			headers: map[string]any{messaging.HeaderContentType: "text/\xff"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "id-1",
				Payload: []byte("body"),
			})
			if tc.headers != nil {
				env.StampHeaders(tc.headers)
			}

			pub, err := PublishFromEnvelope(env, tc.topic, SenderOptions{QoS: 1}, nil)
			require.Error(t, err)
			require.ErrorIs(t, err, shared.ErrInvalidPayload)
			require.Nil(t, pub)
		})
	}

	t.Run("binary_correlation_data_is_not_utf8_checked", func(t *testing.T) {
		// Correlation Data is BINARY on the wire, so arbitrary bytes are legal
		// and must survive — the identity of a producer that uses binary
		// correlation depends on it.
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "id-1",
			Payload: []byte("body"),
		})
		env.StampHeaders(map[string]any{
			messaging.HeaderCorrelationData: base64.RawURLEncoding.EncodeToString([]byte{0x00, 0xff, 0xfe}),
		})

		pub, err := PublishFromEnvelope(env, "t/out", SenderOptions{QoS: 1}, nil)
		require.NoError(t, err)
		require.Equal(t, []byte{0x00, 0xff, 0xfe}, pub.Properties.CorrelationData)
	})

	t.Run("legal_non_ascii_still_passes", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "id-1",
			Payload: []byte("body"),
		})
		env.StampHeaders(map[string]any{"x-app-city": "Malmö"})

		pub, err := PublishFromEnvelope(env, "t/out", SenderOptions{QoS: 1}, nil)
		require.NoError(t, err)
		require.NotNil(t, pub)
	})
}

// TestPublishFromEnvelope_AdmitsFieldsAtTheWireLimit is the other half of the
// boundary: exactly 65,535 bytes encodes losslessly and must still be sent.
func TestPublishFromEnvelope_AdmitsFieldsAtTheWireLimit(t *testing.T) {
	atLimit := strings.Repeat("x", mqttStringFieldLimit)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "id-1",
		Payload: []byte("body"),
	})
	env.StampHeaders(map[string]any{"x-app-note": atLimit})

	pub, err := PublishFromEnvelope(env, "t/out", SenderOptions{QoS: 1}, nil)
	require.NoError(t, err)
	require.NotNil(t, pub)

	var found string
	for _, u := range pub.Properties.User {
		if u.Key == "x-app-note" {
			found = u.Value
		}
	}
	require.Len(t, found, mqttStringFieldLimit, "a value at the limit must survive intact")
}

// TestPublishFromEnvelope_RejectionIsCountedAndNeverReachesTheBroker pins that
// the seam counts the refusal and does not call the SDK. The nil
// ConnectionManager is deliberate: a publish that slipped through would panic.
func TestPublishFromEnvelope_RejectionIsCountedAndNeverReachesTheBroker(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "id-1",
		Payload: []byte("body"),
	})
	env.StampHeaders(map[string]any{"x-app-note": strings.Repeat("x", mqttFieldLimitOverflow)})

	rec := &ports.RecordingExporter{}
	conn := &pahoConn{metrics: rec}

	_, err := conn.PublishEnvelope(t.Context(), env, "t/out", SenderOptions{QoS: 1}, nil)
	require.ErrorIs(t, err, shared.ErrPayloadTooLarge)
	require.Len(t, rec.FindEntries(MetricMQTTEgressRejected), 1)
}

// TestPublishFromEnvelope_ExpiredEnvelopeStillCarriesBrokerExpiry pins the
// expiry race. The route checks expiry, then the packet is built; an envelope
// whose remaining TTL runs out in between was previously published with NO
// Message Expiry Interval, so the broker retained it for a queued subscriber
// indefinitely — the strictly worse outcome of the two. MQTT v5 has no
// "already expired" encoding (the interval is whole seconds and zero means "no
// expiry"), so the local decision to send is honoured and the interval clamps
// to one second: the broker discards it at the next opportunity.
func TestPublishFromEnvelope_ExpiredEnvelopeStillCarriesBrokerExpiry(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	build := func(t *testing.T, expiry time.Time, at time.Time) *uint32 {
		t.Helper()
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:        "id-1",
			Payload:   []byte("body"),
			CreatedAt: base,
		})
		require.NoError(t, env.SetExpiry(expiry))

		pub, err := PublishFromEnvelope(env, "t/out", SenderOptions{QoS: 1}, clocktest.NewAt(at))
		require.NoError(t, err)
		require.NotNil(t, pub.Properties)
		return pub.Properties.MessageExpiry
	}

	t.Run("already_expired_clamps_to_one_second", func(t *testing.T) {
		got := build(t, base.Add(30*time.Second), base.Add(90*time.Second))
		require.NotNil(t, got, "an expired envelope must never be published without broker expiry")
		require.Equal(t, uint32(1), *got)
	})

	t.Run("expiring_exactly_now_clamps_to_one_second", func(t *testing.T) {
		got := build(t, base.Add(30*time.Second), base.Add(30*time.Second))
		require.NotNil(t, got)
		require.Equal(t, uint32(1), *got)
	})

	t.Run("sub_second_remainder_clamps_to_one_second", func(t *testing.T) {
		got := build(t, base.Add(30*time.Second), base.Add(30*time.Second-time.Millisecond))
		require.NotNil(t, got)
		require.Equal(t, uint32(1), *got)
	})

	t.Run("live_remainder_is_whole_seconds", func(t *testing.T) {
		got := build(t, base.Add(90*time.Second), base.Add(30*time.Second))
		require.NotNil(t, got)
		require.Equal(t, uint32(60), *got)
	})

	t.Run("no_expiry_advertises_none", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:        "id-1",
			Payload:   []byte("body"),
			CreatedAt: base,
		})
		pub, err := PublishFromEnvelope(env, "t/out", SenderOptions{QoS: 1}, clocktest.NewAt(base))
		require.NoError(t, err)
		if pub.Properties != nil {
			require.Nil(t, pub.Properties.MessageExpiry)
		}
	})
}

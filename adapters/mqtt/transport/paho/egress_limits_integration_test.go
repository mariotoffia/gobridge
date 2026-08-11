package paho

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// brokerMaximumPacketSizeLimit is the ceiling the broker in this test grants.
// It is comfortably above an empty PUBLISH and comfortably below the oversized
// one, so neither outcome depends on exact header arithmetic.
const brokerMaximumPacketSizeLimit = 2048

// TestMQTTEgress_BrokerMaximumPacketSizeRejectsBeforeTheWire pins the egress
// ceiling against a real broker that actually grants one. Mosquitto advertises
// max_packet_size in its CONNACK; a client that ignores it and writes a larger
// packet is answered with a DISCONNECT, which leaves QoS 1 completion ambiguous
// and recycles the session on every retry.
//
// The proof is three-part: the oversized publish is refused locally with a
// permanent classification, the session is still healthy afterwards (no
// broker-side disconnect happened), and an ordinary publish on the same session
// still succeeds — so the refusal cost nothing but the one rejected message.
func TestMQTTEgress_BrokerMaximumPacketSizeRejectsBeforeTheWire(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	const topic = "egress-limits/maximum-packet-size"
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithExtraConfig("max_packet_size 2048\n"),
	)
	t.Cleanup(broker.Stop)

	metrics := &ports.RecordingExporter{}
	session := NewSession(SessionOptions{
		BrokerURLs:     []string{broker.URL()},
		ClientID:       mqttlocal.UniqueClientID("egress-packet-limit"),
		ConnectTimeout: 10 * time.Second,
		KeepAlive:      30,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil, metrics)
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	require.NoError(t, session.Start(ctx))

	require.Equal(t, uint32(brokerMaximumPacketSizeLimit), session.brokerMaximumPacketSize(),
		"the CONNACK ceiling must be captured for the connection that will publish")

	sender := NewSender(session, SenderOptions{QoS: 1, Timeout: 10 * time.Second})

	oversized := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "egress-oversized",
		Payload: []byte(strings.Repeat("x", brokerMaximumPacketSizeLimit*2)),
	})
	err := sender.Send(ctx, ports.OutboundMessage{Envelope: oversized, Address: topic})
	require.Error(t, err)
	require.ErrorIs(t, err, shared.ErrPayloadTooLarge,
		"an over-limit packet must be refused locally, never written and disconnected")
	require.Len(t, metrics.FindEntries(MetricMQTTEgressRejected), 1)

	health := session.Health(ctx)
	assert.True(t, health.Ready,
		"refusing the packet must leave the connection untouched; a broker DISCONNECT would not")
	assert.NoError(t, health.LastError)

	admitted := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "egress-admitted",
		Payload: []byte("ok"),
	})
	require.NoError(t, sender.Send(ctx, ports.OutboundMessage{Envelope: admitted, Address: topic}),
		"the session must still be usable after the rejection")
	require.Len(t, metrics.FindEntries(MetricMQTTEgressRejected), 1,
		"an admitted publish must not count as a rejection")
}

// TestMQTTEgress_OverlongFieldRejectsAgainstARealBroker pins the field-limit
// half end to end: a header value past the MQTT v5 65,535-byte ceiling fits
// inside a permissive broker's packet limit, so only the local check stops it.
// Without that check Paho truncates the value and the broker acknowledges
// metadata that differs from the source.
func TestMQTTEgress_OverlongFieldRejectsAgainstARealBroker(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	const topic = "egress-limits/field-limit"
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)

	broker := mqttlocal.NewBrokerInstance(t)
	t.Cleanup(broker.Stop)

	metrics := &ports.RecordingExporter{}
	session := NewSession(SessionOptions{
		BrokerURLs:     []string{broker.URL()},
		ClientID:       mqttlocal.UniqueClientID("egress-field-limit"),
		ConnectTimeout: 10 * time.Second,
		KeepAlive:      30,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil, metrics)
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	require.NoError(t, session.Start(ctx))

	sender := NewSender(session, SenderOptions{QoS: 1, Timeout: 10 * time.Second})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "egress-overlong-field",
		Payload: []byte("body"),
	})
	env.StampHeaders(map[string]any{
		messaging.HeaderTenantID: strings.Repeat("t", mqttStringFieldLimit+1),
	})

	err := sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: topic})
	require.ErrorIs(t, err, shared.ErrPayloadTooLarge)
	require.Len(t, metrics.FindEntries(MetricMQTTEgressRejected), 1)

	health := session.Health(ctx)
	assert.True(t, health.Ready)
}

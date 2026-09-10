package paho

import (
	"context"
	"io"
	"math"
	"strings"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// countingWriter records how many bytes the SDK encoder produced without
// keeping the encoded packet.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// TestEncodedPublishSize_MatchesSDKEncoder pins the arithmetic size model
// against the SDK's own encoder. The egress ceiling must be checked BEFORE the
// packet is written, so the size cannot be measured by writing it; this test is
// what stops the arithmetic from drifting away from packets.Publish.Buffers as
// the SDK or the property set evolves.
func TestEncodedPublishSize_MatchesSDKEncoder(t *testing.T) {
	expiry := uint32(60)
	format := byte(1)
	alias := uint16(7)
	subscriptionID := 12345

	cases := map[string]*pahov5.Publish{
		"bare_qos0": {
			Topic:   "a/b",
			QoS:     0,
			Payload: []byte("hello"),
		},
		"qos1_packet_identifier": {
			Topic:    "a/b/c",
			QoS:      1,
			PacketID: 42,
			Payload:  []byte("hello"),
		},
		"every_publish_property": {
			Topic:    strings.Repeat("t", 300),
			QoS:      2,
			PacketID: 9,
			Payload:  make([]byte, 5000),
			Properties: &pahov5.PublishProperties{
				PayloadFormat:          &format,
				MessageExpiry:          &expiry,
				ContentType:            "application/json",
				ResponseTopic:          "reply/here",
				CorrelationData:        []byte{0x00, 0xff, 0x10},
				TopicAlias:             &alias,
				SubscriptionIdentifier: &subscriptionID,
				User: []pahov5.UserProperty{
					{Key: "k1", Value: "v1"},
					{Key: strings.Repeat("k", 400), Value: strings.Repeat("v", 900)},
				},
			},
		},
		"properties_length_crosses_variable_byte_boundary": {
			Topic: "x",
			QoS:   0,
			Properties: &pahov5.PublishProperties{
				User: []pahov5.UserProperty{
					{Key: strings.Repeat("k", 200), Value: strings.Repeat("v", 200)},
				},
			},
		},
	}

	for name, pub := range cases {
		t.Run(name, func(t *testing.T) {
			var counter countingWriter
			written, err := pub.Packet().ToControlPacket().WriteTo(&counter)
			require.NoError(t, err)
			require.Equal(t, written, counter.n)

			require.Equal(t, uint64(written), encodedPublishSize(pub),
				"arithmetic size model must equal the bytes the SDK encoder writes")
		})
	}
}

func TestEncodedPublishSize_NilPublishIsZero(t *testing.T) {
	require.Equal(t, uint64(0), encodedPublishSize(nil))
}

// TestPublishEnvelope_RejectsPacketAboveBrokerMaximumPacketSize is the egress
// half of the MQTT v5 Maximum Packet Size contract: the broker grants a ceiling
// in its CONNACK and a client MUST NOT send a larger packet. Neither Paho nor
// autopaho validates it, so an over-limit publish previously reached the socket
// and the broker answered with a DISCONNECT — ambiguous completion for QoS 1/2,
// a false local success for QoS 0, and session churn on every retry.
//
// The nil ConnectionManager is load-bearing: the rejection must happen before
// any SDK call, so a publish that slipped through would panic instead of
// silently passing.
func TestPublishEnvelope_RejectsPacketAboveBrokerMaximumPacketSize(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "id-1",
		Payload: make([]byte, 4096),
	})

	t.Run("over_limit_is_rejected_before_the_wire", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		conn := &pahoConn{
			metrics:             rec,
			brokerMaxPacketSize: func() uint32 { return 1024 },
		}

		_, err := conn.PublishEnvelope(context.Background(), env, "t/out", SenderOptions{QoS: 1}, nil)

		require.Error(t, err)
		require.ErrorIs(t, err, shared.ErrPayloadTooLarge)
		require.Len(t, rec.FindEntries(MetricMQTTEgressRejected), 1)
	})

	t.Run("classified_rejection_survives_sender_mapping", func(t *testing.T) {
		conn := &pahoConn{brokerMaxPacketSize: func() uint32 { return 1024 }}
		_, err := conn.PublishEnvelope(context.Background(), env, "t/out", SenderOptions{QoS: 1}, nil)
		require.Error(t, err)

		// Sender.Send funnels every seam error through MapError. A permanent
		// classification that MapError downgraded to ErrUnavailable would make
		// the route retry an over-limit packet forever.
		mapped := MapError(err)
		require.ErrorIs(t, mapped, shared.ErrPayloadTooLarge)
		require.Equal(t, shared.ErrorRejected, mapped.Class)
	})

	t.Run("absent_broker_limit_still_enforces_the_protocol_ceiling", func(t *testing.T) {
		// No CONNACK grant means no BROKER limit, but MQTT v5 caps a packet at
		// 256 MiB - 1: a larger Remaining Length cannot be encoded, so the
		// packet is unsendable whatever the broker said.
		require.NoError(t, enforceEgressPacketLimit(&pahov5.Publish{
			Topic:   "t",
			Payload: make([]byte, 1<<20),
		}, 0))

		oversizedPayload := make([]byte, mqttMaxPacketSize)
		require.Error(t, enforceEgressPacketLimit(&pahov5.Publish{
			Topic:   "t",
			Payload: oversizedPayload,
		}, 0))
	})

	t.Run("broker_grant_above_the_protocol_ceiling_is_capped", func(t *testing.T) {
		require.Error(t, enforceEgressPacketLimit(&pahov5.Publish{
			Topic:   "t",
			Payload: make([]byte, mqttMaxPacketSize),
		}, math.MaxUint32))
	})

	t.Run("packet_at_exactly_the_limit_is_admitted", func(t *testing.T) {
		pub := &pahov5.Publish{Topic: "t", Payload: make([]byte, 64)}
		size := encodedPublishSize(pub)
		require.NoError(t, enforceEgressPacketLimit(pub, uint32(size)))
		require.Error(t, enforceEgressPacketLimit(pub, uint32(size)-1))
	})
}

// TestSessionBrokerMaximumPacketSize_CapturedPerConnectionGeneration pins that
// the ceiling is taken from the CONNACK of the connection that is actually
// publishing. autopaho reconnects underneath the session, and a resumed or
// relocated broker can grant a different limit, so a value captured once at
// Start would be stale.
func TestSessionBrokerMaximumPacketSize_CapturedPerConnectionGeneration(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://localhost:1883"},
		ClientID:   "limit-generation",
	}, connectivity.SessionEphemeral, nil)

	require.Equal(t, uint32(0), s.brokerMaximumPacketSize(),
		"before any CONNACK the adapter knows of no broker ceiling")

	s.mu.Lock()
	generation := s.connectionGeneration
	s.mu.Unlock()

	s.recordBrokerMaxPacketSize(generation, 4096)
	require.Equal(t, uint32(4096), s.brokerMaximumPacketSize())

	// A callback from a DISCARDED ConnectionManager must never overwrite the
	// live generation's ceiling.
	s.recordBrokerMaxPacketSize(generation-1, 16)
	require.Equal(t, uint32(4096), s.brokerMaximumPacketSize())

	// A reconnect on the same generation re-grants the ceiling.
	s.recordBrokerMaxPacketSize(generation, 2048)
	require.Equal(t, uint32(2048), s.brokerMaximumPacketSize())
}

func TestConnackMaximumPacketSize(t *testing.T) {
	require.Equal(t, uint32(0), connackMaximumPacketSize(nil))
	require.Equal(t, uint32(0), connackMaximumPacketSize(&pahov5.Connack{}))
	require.Equal(t, uint32(0), connackMaximumPacketSize(&pahov5.Connack{
		Properties: &pahov5.ConnackProperties{},
	}), "an absent Maximum Packet Size means the broker set no limit")

	limit := uint32(65536)
	require.Equal(t, limit, connackMaximumPacketSize(&pahov5.Connack{
		Properties: &pahov5.ConnackProperties{MaximumPacketSize: &limit},
	}))
}

var _ io.Writer = (*countingWriter)(nil)

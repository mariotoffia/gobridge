package paho

import (
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// advertisedMaximumPacket builds a QoS 1 PUBLISH with no payload whose
// metadata section is filled with User Properties until the packet is exactly
// the Maximum Packet Size the CONNECT advertises for maxPayloadBytes. The
// broker enforces only that whole-packet limit, so a compliant broker forwards
// the packet and every byte of it reaches the decoder. With valueBytes zero the
// properties are the five-byte minimum and the count runs into the tens of
// thousands; with a large valueBytes the count stays under the retained cap
// and the bytes go into the values instead.
func advertisedMaximumPacket(t testing.TB, maxPayloadBytes uint32, valueBytes int) (packet []byte, userProperties int) {
	t.Helper()
	wire, err := wirePacketSizeFor(maxPayloadBytes)
	require.NoError(t, err)
	// Fixed header, three-byte Remaining Length, two-byte topic length, the
	// topic itself, the QoS 1 packet identifier and a three-byte properties
	// length. The topic is padded so the packet lands on the limit exactly.
	topic := "t/max"
	overhead := 1 + 3 + 2 + len(topic) + 2 + 3
	property := testRawUserProperty("", strings.Repeat("v", valueBytes))
	userProperties = (int(wire) - overhead) / len(property)
	topic += strings.Repeat("p", int(wire)-overhead-userProperties*len(property))
	properties := make([]byte, 0, userProperties*len(property))
	for range userProperties {
		properties = append(properties, property...)
	}
	packet = testPublishPacket(1, topic, properties, nil)
	require.Len(t, packet, int(wire), "the packet must sit exactly on the advertised Maximum Packet Size")
	return packet, userProperties
}

// TestIngressLimits_PacketAtAdvertisedMaximumIsRefusedByTheCallback covers the
// worst legal packets a compliant broker forwards: zero payload, and a
// metadata section that fills the advertised Maximum Packet Size either with
// minimum-size User Properties or with the retained cap of large ones. The
// predecode guard must hand the SDK at most one User Property above the
// retained cap, and the callback must still refuse the packet for the cap it
// breaks, so it is acked and dropped instead of retained.
func TestIngressLimits_PacketAtAdvertisedMaximumIsRefusedByTheCallback(t *testing.T) {
	tests := []struct {
		name          string
		valueBytes    int
		wantDecoded   int
		wantViolation string
	}{
		{
			name:          "maximum count of minimum-size properties",
			valueBytes:    0,
			wantDecoded:   maxDecodedUserProperties,
			wantViolation: "user_properties",
		},
		{
			name:          "retained cap of properties filling the metadata allowance",
			valueBytes:    3_065,
			wantDecoded:   maxIngressUserProperties,
			wantViolation: "metadata",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet, sent := advertisedMaximumPacket(t, DefaultMaxPayloadBytes, test.valueBytes)
			require.GreaterOrEqual(t, sent, test.wantDecoded)

			guarded := newMQTTIngressConn(newTestNetConn(packet, 0), uint32(len(packet)), nil)
			control, err := packets.ReadPacket(guarded)
			require.NoError(t, err)
			publish := pahov5.PublishFromPacketPublish(control.Content.(*packets.Publish))

			require.Len(t, publish.Properties.User, test.wantDecoded,
				"the guard must bound the User Properties the SDK decodes to one above the retained cap")
			class, violation := newRouter(nil, nil).ingressCapViolation(publish)
			require.Error(t, violation, "a packet at the advertised maximum must still be refused by the callback")
			assert.Equal(t, test.wantViolation, class)
		})
	}
}

package paho

import (
	"runtime"
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
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

// decodeMeasurement is what one SDK decode of a guarded packet cost: the bytes
// it allocated while running and the bytes its decoded representation keeps
// alive afterwards.
type decodeMeasurement struct {
	allocated uint64
	retained  int64
	publish   *pahov5.Publish
}

// measureGuardedDecode feeds packet through the predecode guard into the SDK
// decoder the way a live connection does and reports the cheapest of a few
// runs, so allocation by anything else in the process can only inflate a
// single sample, never the result. The first run primes the guard's reusable
// frame buffer so the measurement sees only what ONE decode allocates.
func measureGuardedDecode(t *testing.T, packet []byte) decodeMeasurement {
	t.Helper()
	const runs = 5
	wire := make([]byte, 0, len(packet)*(runs+1))
	for range runs + 1 {
		wire = append(wire, packet...)
	}
	guarded := newMQTTIngressConn(newTestNetConn(wire, 0), uint32(len(packet)), nil)
	_, err := packets.ReadPacket(guarded)
	require.NoError(t, err)

	best := decodeMeasurement{allocated: ^uint64(0), retained: int64(^uint64(0) >> 1)}
	for range runs {
		var before, after, afterGC runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		control, readErr := packets.ReadPacket(guarded)
		var publish *pahov5.Publish
		if readErr == nil {
			publish = pahov5.PublishFromPacketPublish(control.Content.(*packets.Publish))
		}
		runtime.ReadMemStats(&after)
		runtime.GC()
		runtime.ReadMemStats(&afterGC)
		require.NoError(t, readErr)

		allocated := after.TotalAlloc - before.TotalAlloc
		retained := int64(afterGC.HeapAlloc) - int64(before.HeapAlloc)
		if allocated < best.allocated {
			best.allocated = allocated
		}
		if retained < best.retained {
			best.retained = retained
		}
		best.publish = publish
		runtime.KeepAlive(publish)
	}
	return best
}

// TestIngressMemory_ZeroPayloadPacketAtAdvertisedMaximum_DecodesWithinCrossingSlot
// pins the crossing slot against the SDK's real behaviour for the worst legal
// packets. The CONNECT advertises only a whole-packet Maximum Packet Size, so
// a zero-payload packet at that limit can carry tens of thousands of five-byte
// User Properties, each of which the SDK would materialise twice before the
// publish callback could refuse the packet; or it can carry the retained cap
// of large properties whose every byte the SDK copies out of its read buffer.
// The guard has to bound what the decoder sees to one entry above the retained
// cap, and the crossing slot — one raw wire packet plus everything one decode
// allocates — has to hold either shape with the raw packet included, while the
// decoded representation has to fit the retained packet slot.
func TestIngressMemory_ZeroPayloadPacketAtAdvertisedMaximum_DecodesWithinCrossingSlot(t *testing.T) {
	crossing, err := ingressMemoryCrossingBytes(DefaultMaxPayloadBytes)
	require.NoError(t, err)
	retainedBudget, err := decodedPacketSizeFor(DefaultMaxPayloadBytes)
	require.NoError(t, err)

	tests := []struct {
		name          string
		valueBytes    int
		wantDecoded   int
		wantViolation string
		// allocationBounded asserts the decode's allocation VOLUME against the
		// crossing slot. It is the measure of the property-count amplification
		// (one allocation per decoded property, twice), so it is asserted for
		// the minimum-size shape. The large-value shape is dominated by the
		// SDK's read-buffer growth, whose dead temporaries and race-detector
		// instrumentation inflate the volume well past what is ever live at
		// once; that shape pins the retained representation instead, and the
		// finite-cgroup proof measures its live peak.
		allocationBounded bool
	}{
		{
			name:              "maximum count of minimum-size properties",
			valueBytes:        0,
			wantDecoded:       maxDecodedUserProperties,
			wantViolation:     "user_properties",
			allocationBounded: true,
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

			measured := measureGuardedDecode(t, packet)
			t.Logf("wire=%d sent=%d allocated=%d retained=%d crossing=%d retainedBudget=%d",
				len(packet), sent, measured.allocated, measured.retained, crossing, retainedBudget)

			require.Len(t, measured.publish.Properties.User, test.wantDecoded,
				"the guard must bound the User Properties the SDK decodes to one above the retained cap")
			if test.allocationBounded {
				require.LessOrEqual(t, uint64(len(packet))+measured.allocated, crossing,
					"raw packet %d + decode allocation %d must fit the crossing slot %d",
					len(packet), measured.allocated, crossing)
			}
			require.LessOrEqual(t, measured.retained, int64(retainedBudget),
				"the decoded representation %d must fit the retained packet slot %d",
				measured.retained, retainedBudget)

			class, violation := newRouter(nil, nil).ingressCapViolation(measured.publish)
			require.Error(t, violation, "a packet at the advertised maximum must still be refused by the callback")
			require.Equal(t, test.wantViolation, class)
			runtime.KeepAlive(measured.publish)
		})
	}
}

// TestIngressMemory_MaxPayloadPacket_RetainsWithinPacketSlot pins the other
// worst case of the per-slot budget: a packet carrying the full max_payload_bytes
// body. Its decoded representation — the SDK copies the payload out of its
// read buffer with slack — must fit the retained packet slot that every window
// position is charged.
func TestIngressMemory_MaxPayloadPacket_RetainsWithinPacketSlot(t *testing.T) {
	payload := make([]byte, DefaultMaxPayloadBytes)
	for i := range payload {
		payload[i] = byte(i)
	}
	packet := testPublishPacket(1, "t/payload", testUserProperties(1), payload)
	retainedBudget, err := decodedPacketSizeFor(DefaultMaxPayloadBytes)
	require.NoError(t, err)

	measured := measureGuardedDecode(t, packet)

	require.Equal(t, payload, measured.publish.Payload)
	require.LessOrEqual(t, measured.retained, int64(retainedBudget),
		"the decoded representation %d must fit the retained packet slot %d",
		measured.retained, retainedBudget)
	runtime.KeepAlive(measured.publish)
}

package paho

import (
	"testing"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"
)

// TestMinEncodedUserProperty_MatchesTheWireEncoding pins the divisor the
// worst-case property count is derived from: the smallest User Property a
// broker can legally forward is the one-byte identifier plus two empty
// length-prefixed strings. If the SDK ever encoded it in fewer bytes the
// derived count would be too low and the memory budget would undercount again.
func TestMinEncodedUserProperty_MatchesTheWireEncoding(t *testing.T) {
	properties := &packets.Properties{User: []packets.User{{Key: "", Value: ""}}}
	require.Equal(t, int(minEncodedUserPropertyBytes), len(properties.Pack(packets.PUBLISH)))
}

// TestIngressMemoryCrossing_CoversMaximumCountTinyProperties is the memory
// accounting fix. The CONNECT advertises only a whole-packet Maximum Packet
// Size, so a compliant broker may fill the entire metadata allowance with
// minimum-size User Properties — tens of thousands of them, not the 128 the
// router retains. Paho materialises every one of them (wire packet and callback
// copy) before the callback can ack-and-drop the violation, so the ONE decode in
// flight per connection must be budgeted for the wire worst case, not for the
// retained cap.
//
// The split is deliberate: only the crossing slot pays for the amplification.
// onPublishReceived rejects an over-cap packet before anything retains it, so
// the per-slot retained budget stays at the router's cap and the per-session
// bound does not explode by three orders of magnitude.
func TestIngressMemoryCrossing_CoversMaximumCountTinyProperties(t *testing.T) {
	const maxPayload = uint32(256 << 10)

	require.Greater(t, maxWireUserProperties, uint64(maxIngressUserProperties)*100,
		"a legal metadata section holds far more properties than the retained cap")
	require.Equal(t, mqttPacketOverheadAllowance/minEncodedUserPropertyBytes, maxWireUserProperties)

	wire, err := wirePacketSizeFor(maxPayload)
	require.NoError(t, err)
	crossing, err := maxPacketSizeFor(maxPayload)
	require.NoError(t, err)

	worstCaseDecode := uint64(wire) +
		maxWireUserProperties*retainedUserPropertyBytes +
		retainedPacketFixedBytes
	require.GreaterOrEqual(t, uint64(crossing), uint64(wire)+worstCaseDecode,
		"the crossing slot must hold the raw wire packet plus its worst-case decoded form")

	// The retained per-slot budget stays at the router's cap: an over-cap packet
	// is acked and dropped, never queued.
	retained, err := decodedPacketSizeFor(maxPayload)
	require.NoError(t, err)
	require.Equal(t,
		uint64(wire)+uint64(maxIngressUserProperties)*retainedUserPropertyBytes+retainedPacketFixedBytes,
		uint64(retained))
	require.Less(t, uint64(retained), uint64(crossing))
}

// TestIngressMemoryBound_DefaultProfileStillFitsItsDefaultBudget guards the
// consequence of the larger crossing slot: the shipped defaults must remain
// admissible, otherwise every default deployment fails validation at startup.
func TestIngressMemoryBound_DefaultProfileStillFitsItsDefaultBudget(t *testing.T) {
	bound, err := IngressMemoryBound(DefaultMaxPayloadBytes, DefaultReceiveMaximum, 100)
	require.NoError(t, err)
	require.LessOrEqual(t, bound, DefaultIngressMemoryBudgetBytes)

	// And a budget below one crossing slot is still rejected rather than
	// silently admitting a session that cannot decode a single legal packet.
	crossing, err := ingressMemoryCrossingBytes(DefaultMaxPayloadBytes)
	require.NoError(t, err)
	_, err = LargestSafeReceiveMaximum(DefaultMaxPayloadBytes, crossing-1, 0)
	require.Error(t, err)
}

// TestRouterIngressMemory_MaximumCountTinyPropertiesAckDropped pins the runtime
// half against the accounting: a packet carrying far more than the retained cap
// of minimum-size properties is acked and dropped by the callback, so it never
// occupies a retained slot.
func TestRouterIngressMemory_MaximumCountTinyPropertiesAckDropped(t *testing.T) {
	r := newRouter(nil, nil, withMaxPayloadBytes(64))

	user := make([]pahov5.UserProperty, maxIngressUserProperties+1)
	pub := &pahov5.Publish{
		Topic:      "t/in",
		Payload:    []byte("x"),
		Properties: &pahov5.PublishProperties{User: user},
	}

	class, violation := r.ingressCapViolation(pub)
	require.Error(t, violation)
	require.Equal(t, "user_properties", class)
}

package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"
)

// TestIngressMemoryCrossing_BudgetsTheTruncatedDecode pins the accounting the
// predecode guard makes possible. The CONNECT advertises only a whole-packet
// Maximum Packet Size, so a compliant broker may fill the entire metadata
// allowance with minimum-size User Properties — tens of thousands of them.
// The guard cuts that list to one entry above the retained cap before the SDK
// decodes the packet, so the ONE decode in flight is budgeted for that count,
// not for the wire worst case, and the SDK's wire-sized buffers are what
// dominate the crossing slot instead.
func TestIngressMemoryCrossing_BudgetsTheTruncatedDecode(t *testing.T) {
	const maxPayload = uint32(256 << 10)

	require.Equal(t, maxIngressUserProperties+1, maxDecodedUserProperties,
		"the decoder sees exactly one property above the cap: enough to refuse the packet, no more")

	wire, err := wirePacketSizeFor(maxPayload)
	require.NoError(t, err)
	crossing, err := maxPacketSizeFor(maxPayload)
	require.NoError(t, err)

	worstCaseDecode := sdkDecodeWireMultiple*uint64(wire) +
		maxDecodedUserProperties*retainedUserPropertyBytes +
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
// consequence of the crossing slot: the shipped defaults must remain
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
// half against the accounting: a packet carrying more than the retained cap
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

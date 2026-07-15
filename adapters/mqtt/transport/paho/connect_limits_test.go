package paho

import (
	"math"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// c-mempkt (MEDIUM): the documented worst-case pending memory
// `receive_maximum × max_payload` is only broker-ENFORCEABLE if the CONNECT
// advertises a Maximum Packet Size. Without it a large-message broker can push
// receive_maximum × multi-MiB into the grace-window buffer (multiple GiB).
//
// Fix: applyConnectLimits advertises MaximumPacketSize derived from the
// configured max_payload_bytes plus a bounded protocol-overhead allowance, so
// the broker MUST NOT deliver a packet larger than the adapter's per-message
// ceiling. Unset (0) leaves it absent (no invented cap; prior behaviour).
// ═══════════════════════════════════════════════════════════════════════════

// TestApplyConnectLimits pins the CONNECT properties this adapter advertises.
// The ConnectPacketBuilder in dial() delegates verbatim to applyConnectLimits,
// so asserting the helper's output is asserting the CONNECT packet contents.
//
// Mutation killed: dropping the MaximumPacketSize assignment in applyConnectLimits
// (advertise nothing) leaves cp.Properties.MaximumPacketSize nil → the
// "payload_set" / "receive_maximum_unset_payload_set" cases fail. Setting it to
// maxPayloadBytes with NO overhead makes MaximumPacketSize == max_payload_bytes,
// failing the "a payload of exactly max_payload_bytes is still admitted" check.
func TestApplyConnectLimits(t *testing.T) {
	t.Run("both_set", func(t *testing.T) {
		cp := &pahov5.Connect{}
		require.NoError(t, applyConnectLimits(cp, 1024, 1<<20)) // rm=1024, max_payload=1 MiB

		require.NotNil(t, cp.Properties)
		require.NotNil(t, cp.Properties.ReceiveMaximum)
		require.Equal(t, uint16(1024), *cp.Properties.ReceiveMaximum)

		require.NotNil(t, cp.Properties.MaximumPacketSize,
			"a configured max_payload_bytes must advertise a Maximum Packet Size")
		want := uint32(1<<20) + uint32(mqttPacketOverheadAllowance)
		require.Equal(t, want, *cp.Properties.MaximumPacketSize,
			"MaximumPacketSize = max_payload_bytes + bounded overhead allowance")
	})

	t.Run("payload_unset_leaves_packet_size_absent", func(t *testing.T) {
		cp := &pahov5.Connect{}
		require.NoError(t, applyConnectLimits(cp, 1024, 0)) // max_payload unset ⇒ no packet-size cap

		require.NotNil(t, cp.Properties)
		require.NotNil(t, cp.Properties.ReceiveMaximum)
		require.Nil(t, cp.Properties.MaximumPacketSize,
			"unset max_payload_bytes must NOT invent a Maximum Packet Size (prior behaviour)")
	})

	t.Run("receive_maximum_unset_payload_set", func(t *testing.T) {
		cp := &pahov5.Connect{}
		require.NoError(t, applyConnectLimits(cp, 0, 4096))

		require.NotNil(t, cp.Properties)
		require.Nil(t, cp.Properties.ReceiveMaximum,
			"a zero receive_maximum leaves the property unset")
		require.NotNil(t, cp.Properties.MaximumPacketSize)
		require.Equal(t, uint32(4096)+uint32(mqttPacketOverheadAllowance), *cp.Properties.MaximumPacketSize)
	})

	t.Run("neither_set_leaves_properties_nil", func(t *testing.T) {
		cp := &pahov5.Connect{}
		require.NoError(t, applyConnectLimits(cp, 0, 0))
		require.Nil(t, cp.Properties, "no limits configured ⇒ no CONNECT properties added")
	})

	t.Run("exactly_max_payload_is_admitted", func(t *testing.T) {
		// The whole point of the overhead allowance: a payload of exactly
		// max_payload_bytes plus its MQTT headers must fit UNDER the advertised
		// ceiling, so a legitimate maximum-size message is never rejected.
		const maxPayload uint32 = 512 * 1024
		cp := &pahov5.Connect{}
		require.NoError(t, applyConnectLimits(cp, 1024, maxPayload))

		require.NotNil(t, cp.Properties.MaximumPacketSize)
		require.Greater(t, *cp.Properties.MaximumPacketSize, maxPayload,
			"MaximumPacketSize must exceed max_payload_bytes so a full-size payload plus headers is admitted")
	})

	t.Run("payload_that_cannot_fit_metadata_is_rejected", func(t *testing.T) {
		cp := &pahov5.Connect{}
		err := applyConnectLimits(cp, 1024, math.MaxUint32)

		require.Error(t, err)
		require.Nil(t, cp.Properties.MaximumPacketSize,
			"an explicit unsafe payload ceiling is rejected, never silently clamped")
	})
}

// TestMaxPacketSizeFor covers the pure derivation the helper delegates to.
func TestMaxPacketSizeFor(t *testing.T) {
	got, err := maxPacketSizeFor(0)
	require.NoError(t, err)
	require.Equal(t, uint32(mqttPacketOverheadAllowance), got,
		"a zero payload still reserves the header allowance (callers guard >0)")
	got, err = maxPacketSizeFor(256 << 10)
	require.NoError(t, err)
	require.Equal(t, uint32(256<<10)+uint32(mqttPacketOverheadAllowance), got)
	_, err = maxPacketSizeFor(math.MaxUint32)
	require.Error(t, err, "overflow past the MQTT ceiling is rejected, never clamped or wrapped")
}

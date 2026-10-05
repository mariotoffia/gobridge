package paho

import (
	"fmt"

	"github.com/mariotoffia/gobridge/domain/shared"
)

const (
	// DefaultMaxPayloadBytes is the effective inbound application-payload
	// ceiling when max_payload_bytes is zero.
	DefaultMaxPayloadBytes uint32 = 256 << 10
	// DefaultReceiveMaximum is the effective MQTT v5 Receive Maximum when
	// receive_maximum is zero.
	DefaultReceiveMaximum uint16 = 192
)

// mqttPacketOverheadAllowance is the metadata allowance admitted on top of a
// full max_payload_bytes body, so the advertised Maximum Packet Size is
// max_payload_bytes + this allowance. The 128 KiB covers the MQTT v5 PUBLISH
// fixed-header byte, the worst-case four-byte Remaining Length encoding, the
// two-byte topic-length prefix plus a 65,535-byte topic, a two-byte QoS packet
// identifier, the worst-case four-byte properties-length encoding, and 65,524
// bytes of properties. A packet with a smaller body may use more metadata, but
// Maximum Packet Size still caps the whole packet at max_payload_bytes + this
// allowance.
const mqttPacketOverheadAllowance uint64 = 128 << 10

// mqttMaxPacketSize is the MQTT v5 Maximum Packet Size ceiling: 256 MiB - 1.
const mqttMaxPacketSize uint64 = 268_435_455

const (
	// maxIngressUserProperties caps the User Properties an accepted packet may
	// carry. A packet over the cap is acked and dropped by the callback.
	maxIngressUserProperties = 128
	// maxIngressMetadataBytes caps the encoded topic and properties metadata an
	// accepted packet may carry.
	maxIngressMetadataBytes uint64 = mqttPacketOverheadAllowance
	// maxDecodedUserProperties is the most User Properties the predecode guard
	// lets the SDK decode from one inbound PUBLISH. The CONNECT advertises only
	// a whole-packet Maximum Packet Size, so a compliant broker forwards a
	// packet whose metadata section is nothing but five-byte User Properties —
	// tens of thousands of them at the default payload size — and the SDK
	// would decode every one of them twice before the publish callback could
	// refuse the packet. The guard therefore cuts the list on the raw bytes
	// (truncatePublishUserProperties) to one entry above the retained cap:
	// enough for the callback to see the violation and ack-and-drop the
	// packet. The bound holds for every decoded packet — the one being
	// decoded, the ones the SDK queues ahead of the callback, and the ones the
	// router keeps.
	maxDecodedUserProperties = maxIngressUserProperties + 1
)

// wirePacketSizeFor returns the MQTT v5 Maximum Packet Size advertised to the
// broker.
func wirePacketSizeFor(maxPayloadBytes uint32) (uint32, error) {
	if uint64(maxPayloadBytes) > mqttMaxPacketSize-mqttPacketOverheadAllowance {
		return 0, shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"mqtt: max_payload_bytes %d exceeds the largest value %d that fits the MQTT v5 packet ceiling with metadata overhead",
			maxPayloadBytes, mqttMaxPacketSize-mqttPacketOverheadAllowance,
		))
	}
	return uint32(uint64(maxPayloadBytes) + mqttPacketOverheadAllowance), nil
}

// withIngressDefaults fills the zero-valued ingress limits with their defaults.
func (o SessionOptions) withIngressDefaults() SessionOptions {
	if o.MaxPayloadBytes == 0 {
		o.MaxPayloadBytes = DefaultMaxPayloadBytes
	}
	if o.ReceiveMaximum == 0 {
		o.ReceiveMaximum = DefaultReceiveMaximum
	}
	return o
}

package paho

import (
	"fmt"
	"math"

	"github.com/mariotoffia/gobridge/domain/shared"
)

const (
	// DefaultMaxPayloadBytes is the effective inbound application-payload
	// ceiling when max_payload_bytes is zero.
	DefaultMaxPayloadBytes uint32 = 256 << 10
	// DefaultReceiveMaximum is the effective MQTT v5 Receive Maximum when
	// receive_maximum is zero.
	DefaultReceiveMaximum uint16 = 192
	// DefaultIngressMemoryBudgetBytes is the per-session conservative ingress
	// memory budget when ingress_memory_budget_bytes is zero.
	DefaultIngressMemoryBudgetBytes uint64 = 256 << 20
)

// mqttPacketOverheadAllowance is the maximum metadata allowance admitted in
// addition to a full max_payload_bytes body. The 128 KiB allowance includes the
// MQTT v5 PUBLISH fixed-header byte, the worst-case four-byte Remaining Length
// encoding, the two-byte topic-length prefix plus a 65,535-byte topic, a
// two-byte QoS packet identifier, the worst-case four-byte properties-length
// encoding, and 65,524 bytes of properties. A packet with a smaller body may
// use more metadata, but Maximum Packet Size still caps the whole admitted
// packet at max_payload_bytes + this allowance, so the byte model never
// undercounts an admitted packet.
const mqttPacketOverheadAllowance uint64 = 128 << 10

// mqttMaxPacketSize is the MQTT v5 Maximum Packet Size ceiling: 256 MiB - 1.
const mqttMaxPacketSize uint64 = 268_435_455

const (
	// maxIngressUserProperties bounds per-property Go struct amplification for
	// packets retained beyond the Paho callback.
	maxIngressUserProperties = 128
	// maxIngressMetadataBytes bounds encoded topic/properties metadata retained
	// by an accepted packet.
	maxIngressMetadataBytes uint64 = mqttPacketOverheadAllowance
	// retainedUserPropertyBytes covers the Paho wire-packet and callback
	// UserProperty structs retained simultaneously for one formula slot.
	retainedUserPropertyBytes uint64 = 64
	// retainedPacketFixedBytes covers Publish/Properties structs, accepted
	// Envelope header-map buckets, outbox/queue item state, and allocator
	// page/size-class rounding observed by the finite-cgroup proof.
	retainedPacketFixedBytes uint64 = 32 << 10
	// minEncodedUserPropertyBytes is the smallest legal encoded MQTT v5 User
	// Property: the one-byte identifier plus two empty length-prefixed UTF-8
	// strings.
	minEncodedUserPropertyBytes uint64 = 5
	// maxWireUserProperties is the largest number of User Properties a packet
	// the broker may forward can carry. The CONNECT advertises only a
	// whole-packet Maximum Packet Size, so a compliant broker forwards a packet
	// that fills the entire metadata allowance with minimum-size properties —
	// tens of thousands of them, not the maxIngressUserProperties the router
	// retains. Paho materialises every one of them (wire packet and callback
	// copy) before the callback can ack-and-drop the violation, so the ONE
	// decode in flight per connection must be budgeted for this count.
	maxWireUserProperties = mqttPacketOverheadAllowance / minEncodedUserPropertyBytes
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

// decodedPacketSizeFor returns a conservative retained-heap base for one
// ACCEPTED decoded packet representation. An accepted packet has passed the
// router's local caps (onPublishReceived checks them before anything retains
// the packet), so maxIngressUserProperties is the right property budget here.
func decodedPacketSizeFor(maxPayloadBytes uint32) (uint32, error) {
	wire, err := wirePacketSizeFor(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	decoded := uint64(wire) +
		maxIngressUserProperties*retainedUserPropertyBytes +
		retainedPacketFixedBytes
	if decoded > math.MaxUint32 {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: decoded packet memory exceeds uint32")
	}
	return uint32(decoded), nil
}

// transientDecodedPacketSizeFor returns the heap Paho can hold for the ONE
// decoded packet in flight while it consumes a wire packet, INCLUDING a packet
// the router will immediately ack-and-drop. It differs from
// decodedPacketSizeFor by the property budget: a broker may forward a packet
// whose metadata section is entirely minimum-size User Properties, so the
// decode transiently materialises maxWireUserProperties of them, far above the
// retained cap. Only this one slot pays for that amplification — an over-cap
// packet never reaches a retained slot.
func transientDecodedPacketSizeFor(maxPayloadBytes uint32) (uint32, error) {
	wire, err := wirePacketSizeFor(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	decoded := uint64(wire) +
		maxWireUserProperties*retainedUserPropertyBytes +
		retainedPacketFixedBytes
	if decoded > math.MaxUint32 {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: transient decoded packet memory exceeds uint32")
	}
	return uint32(decoded), nil
}

// maxPacketSizeFor returns the crossing-slot base: one complete raw wire packet
// held by mqttIngressConn plus the worst-case decoded representation Paho builds
// while consuming it. A rejected packet retains only the raw half, so the same
// slot also covers a maximum-wire rejection.
func maxPacketSizeFor(maxPayloadBytes uint32) (uint32, error) {
	wire, err := wirePacketSizeFor(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	decoded, err := transientDecodedPacketSizeFor(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	crossing, err := checkedAddUint64(uint64(wire), uint64(decoded))
	if err != nil || crossing > math.MaxUint32 {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: crossing packet memory exceeds uint32")
	}
	return uint32(crossing), nil
}

// ingressMemoryPacketBytes returns
// ceil(decodedPacketSize(maxPayloadBytes) * 1.25).
func ingressMemoryPacketBytes(maxPayloadBytes uint32) (uint64, error) {
	if maxPayloadBytes == 0 {
		maxPayloadBytes = DefaultMaxPayloadBytes
	}
	packetSize, err := decodedPacketSizeFor(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	return addIngressMemoryFactor(uint64(packetSize))
}

// ingressMemoryCrossingBytes returns ceil(maxPacketSize * 1.25) for the single
// raw-plus-decoded crossing slot.
func ingressMemoryCrossingBytes(maxPayloadBytes uint32) (uint64, error) {
	if maxPayloadBytes == 0 {
		maxPayloadBytes = DefaultMaxPayloadBytes
	}
	packetSize, err := maxPacketSizeFor(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	return addIngressMemoryFactor(uint64(packetSize))
}

func addIngressMemoryFactor(packet uint64) (uint64, error) {
	quarter := packet / 4
	if packet%4 != 0 {
		var addErr error
		quarter, addErr = checkedAddUint64(quarter, 1)
		if addErr != nil {
			return 0, addErr
		}
	}
	return checkedAddUint64(packet, quarter)
}

// IngressMemoryBound calculates the conservative per-session byte bound:
//
//	packet   = ceil(decodedPacketSize(maxPayloadBytes) * 1.25)
//	crossing = ceil((wirePacketSize + decodedPacketSize) * 1.25)
//	window   = receiveMaximum + dispatchCapacity + routeMaxInFlight
//	bound    = packet * window + crossing
//
// Dispatch capacity equals the effective Receive Maximum. Zero payload and
// receive values select the adapter defaults.
func IngressMemoryBound(
	maxPayloadBytes uint32,
	receiveMaximum uint16,
	routeMaxInFlight uint64,
) (uint64, error) {
	if receiveMaximum == 0 {
		receiveMaximum = DefaultReceiveMaximum
	}
	packet, err := ingressMemoryPacketBytes(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	crossing, err := ingressMemoryCrossingBytes(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	window, err := checkedAddUint64(uint64(receiveMaximum), uint64(receiveMaximum))
	if err != nil {
		return 0, err
	}
	window, err = checkedAddUint64(window, routeMaxInFlight)
	if err != nil {
		return 0, err
	}
	bound, err := checkedMulUint64(packet, window)
	if err != nil {
		return 0, err
	}
	return checkedAddUint64(bound, crossing)
}

// LargestSafeReceiveMaximum derives the largest legal MQTT v5 Receive Maximum
// whose ingress-memory bound fits budgetBytes for the supplied payload and route
// concurrency. It rejects a budget that cannot fit one receive slot, one
// dispatch slot, the route window, and the current packet.
func LargestSafeReceiveMaximum(
	maxPayloadBytes uint32,
	budgetBytes uint64,
	routeMaxInFlight uint64,
) (uint16, error) {
	packet, err := ingressMemoryPacketBytes(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	crossing, err := ingressMemoryCrossingBytes(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	if budgetBytes == 0 {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: ingress memory budget must be greater than zero")
	}
	if budgetBytes < crossing {
		return 0, shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"mqtt: ingress memory budget %d bytes is too small for the raw/decode crossing slot (requires at least %d bytes)",
			budgetBytes,
			crossing,
		))
	}
	maxWindow := (budgetBytes - crossing) / packet
	minWindow, err := checkedAddUint64(routeMaxInFlight, 2)
	if err != nil {
		return 0, err
	}
	if maxWindow < minWindow {
		minimumBytes, mulErr := checkedMulUint64(packet, minWindow)
		if mulErr != nil {
			return 0, mulErr
		}
		minimumBytes, addErr := checkedAddUint64(minimumBytes, crossing)
		if addErr != nil {
			return 0, addErr
		}
		return 0, shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"mqtt: ingress memory budget %d bytes is too small for one receive packet (requires at least %d bytes)",
			budgetBytes, minimumBytes,
		))
	}
	receive := (maxWindow - routeMaxInFlight) / 2
	if receive > math.MaxUint16 {
		receive = math.MaxUint16
	}
	if receive == 0 {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: ingress memory budget cannot fit a legal Receive Maximum")
	}
	bound, err := IngressMemoryBound(maxPayloadBytes, uint16(receive), routeMaxInFlight)
	if err != nil {
		return 0, err
	}
	if bound > budgetBytes {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: derived Receive Maximum exceeds ingress memory budget")
	}
	return uint16(receive), nil
}

func checkedAddUint64(a, b uint64) (uint64, error) {
	if b > math.MaxUint64-a {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: ingress memory calculation overflows integer addition")
	}
	return a + b, nil
}

func checkedMulUint64(a, b uint64) (uint64, error) {
	if a != 0 && b > math.MaxUint64/a {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: ingress memory calculation overflows integer multiplication")
	}
	return a * b, nil
}

func (o SessionOptions) normalizedIngressMemory() SessionOptions {
	out := o
	if !out.ingressDefaultsApplied {
		out.receiveMaximumExplicit = out.receiveMaximumExplicit || out.ReceiveMaximum != 0
		out.ingressMemoryBudgetExplicit = out.IngressMemoryBudgetBytes != 0
	} else {
		if out.ReceiveMaximum != 0 && out.ReceiveMaximum != DefaultReceiveMaximum {
			out.receiveMaximumExplicit = true
		}
		if out.IngressMemoryBudgetBytes != 0 &&
			out.IngressMemoryBudgetBytes != DefaultIngressMemoryBudgetBytes {
			out.ingressMemoryBudgetExplicit = true
		}
	}
	if out.MaxPayloadBytes == 0 {
		out.MaxPayloadBytes = DefaultMaxPayloadBytes
	}
	if out.ReceiveMaximum == 0 {
		out.ReceiveMaximum = DefaultReceiveMaximum
	}
	if out.IngressMemoryBudgetBytes == 0 {
		out.IngressMemoryBudgetBytes = DefaultIngressMemoryBudgetBytes
	}
	out.ingressDefaultsApplied = true
	return out
}

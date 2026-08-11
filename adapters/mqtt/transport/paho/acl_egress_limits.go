package paho

import (
	"fmt"
	"strings"
	"unicode/utf8"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// mqttStringFieldLimit is the largest value any length-prefixed MQTT v5 field
// can carry: the length prefix is two bytes, so 65,535 bytes is the ceiling for
// a topic, a UTF-8 string property, a User Property key or value, and binary
// Correlation Data.
//
// Paho does not reject a longer value — packets.writeString / writeBinary slice
// it and write the shortened form with no error — so the broker would
// acknowledge metadata that differs from the source. A truncated idempotency
// key stops deduplicating, a truncated tenant id mis-attributes, a truncated
// correlation id breaks the reply path, and a cut multi-byte rune is not even
// valid UTF-8 on the wire. Egress therefore validates every field before the
// packet is built.
const mqttStringFieldLimit = 65535

// mqttFieldNameLimit bounds how much of a User Property key is quoted back in a
// rejection message. The key is operator-facing diagnostics, not evidence, and
// the offending publish may carry arbitrary producer-supplied bytes.
const mqttFieldNameLimit = 64

// validatePublishFieldLimits reports whether every length-prefixed field of pub
// fits the MQTT v5 wire limit. It runs on the constructed packet rather than on
// the envelope so no field can be added later and escape the check.
func validatePublishFieldLimits(pub *pahov5.Publish) error {
	if pub == nil {
		return shared.ErrInvalidPayload.WithMessage("mqtt: nil publish packet")
	}
	if err := checkMQTTStringField("topic", pub.Topic); err != nil {
		return err
	}
	if pub.Properties == nil {
		return nil
	}
	if err := checkMQTTStringField("content type", pub.Properties.ContentType); err != nil {
		return err
	}
	if err := checkMQTTStringField("response topic", pub.Properties.ResponseTopic); err != nil {
		return err
	}
	// Correlation Data is BINARY (MQTT v5 §3.3.2.3.6): any byte sequence is
	// legal, so only its length is checked.
	if err := checkMQTTFieldLimit("correlation data", len(pub.Properties.CorrelationData)); err != nil {
		return err
	}
	for _, u := range pub.Properties.User {
		if err := checkMQTTStringField("user property key", u.Key); err != nil {
			return err
		}
		// Name the offending key only when there IS an offence. This loop runs
		// on every publish, and formatting the diagnostic eagerly would
		// allocate once per user property on the hot path.
		if !mqttStringFieldValid(u.Value) {
			return checkMQTTStringField(userPropertyValueField(u.Key), u.Value)
		}
	}
	return nil
}

// mqttStringFieldValid is the allocation-free predicate every UTF-8 encoded
// MQTT v5 string field must satisfy: within the length limit, well-formed
// UTF-8, and free of U+0000 (MQTT v5 §1.5.4 forbids U+0000 and surrogate code
// points; utf8.ValidString already rejects surrogates).
func mqttStringFieldValid(value string) bool {
	return len(value) <= mqttStringFieldLimit &&
		utf8.ValidString(value) &&
		strings.IndexByte(value, 0) < 0
}

// checkMQTTStringField reports why value is not a legal MQTT v5 string field.
//
// Paho encodes whatever bytes it is given, so an application header carrying
// invalid UTF-8 — from a processor, or from a transport that does not enforce
// it — reaches the broker as a malformed packet. The broker answers with a
// DISCONNECT, which recycles the session for every message that reproduces it,
// so the packet is refused here instead.
func checkMQTTStringField(field, value string) error {
	if mqttStringFieldValid(value) {
		return nil
	}
	if err := checkMQTTFieldLimit(field, len(value)); err != nil {
		return err
	}
	if !utf8.ValidString(value) {
		return shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"mqtt: %s is not valid UTF-8; MQTT v5 requires well-formed UTF-8 encoded strings", field,
		)).With("field", field)
	}
	return shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
		"mqtt: %s contains U+0000, which MQTT v5 forbids in a UTF-8 encoded string", field,
	)).With("field", field)
}

// userPropertyValueField names a User Property value for diagnostics. The key
// is bounded because the offending publish carries arbitrary producer-supplied
// bytes.
func userPropertyValueField(key string) string {
	return fmt.Sprintf("user property %q value", boundedFieldName(key))
}

func checkMQTTFieldLimit(field string, length int) error {
	if length <= mqttStringFieldLimit {
		return nil
	}
	return shared.ErrPayloadTooLarge.WithMessage(fmt.Sprintf(
		"mqtt: %s is %d bytes and exceeds the MQTT v5 field limit of %d bytes; "+
			"the broker would receive a truncated value",
		field, length, mqttStringFieldLimit,
	)).With("field", field).With("field_bytes", length)
}

func boundedFieldName(key string) string {
	if len(key) <= mqttFieldNameLimit {
		return key
	}
	return key[:mqttFieldNameLimit] + "..."
}

// enforceEgressPacketLimit rejects a publish whose encoded size exceeds the
// Maximum Packet Size the broker granted in its CONNACK.
//
// A zero grant means the broker advertised none (MQTT v5 §3.2.2.3.6), which
// still leaves the protocol ceiling of 256 MiB - 1: a larger packet cannot
// encode its Remaining Length at all, so it is refused whatever the broker
// said.
//
// MQTT v5 §3.1.2.11.4 forbids a client from sending a packet above the granted
// limit, and a broker that receives one answers with a DISCONNECT: QoS 1/2
// completion is then ambiguous, QoS 0 has already reported local success, and
// every retry churns the session. Neither Paho nor autopaho checks the limit,
// so this is the only place it is enforced — before the packet reaches the
// socket, with a permanent classification so the route rejects rather than
// retries.
func enforceEgressPacketLimit(pub *pahov5.Publish, maximumPacketSize uint32) error {
	limit := uint64(maximumPacketSize)
	if limit == 0 || limit > mqttMaxPacketSize {
		limit = mqttMaxPacketSize
	}
	size := encodedPublishSize(pub)
	if size <= limit {
		return nil
	}
	return shared.ErrPayloadTooLarge.WithMessage(fmt.Sprintf(
		"mqtt: encoded PUBLISH is %d bytes and exceeds the Maximum Packet Size of %d bytes",
		size, limit,
	)).With("encoded_bytes", size).With("maximum_packet_size", limit)
}

// encodedPublishSize returns the exact number of bytes pub occupies on the
// wire: fixed header, Remaining Length encoding, variable header, properties
// and payload.
//
// It mirrors packets.Publish.Buffers and ControlPacket.WriteTo arithmetically
// instead of packing the packet: the ceiling has to be checked BEFORE the write,
// and packing twice would double the property-serialisation cost of every
// publish on the hot path. egress_packet_limits_test.go pins the arithmetic
// against the SDK encoder for a fully populated publish so the two cannot drift.
func encodedPublishSize(pub *pahov5.Publish) uint64 {
	if pub == nil {
		return 0
	}
	properties := encodedPublishPropertiesSize(pub.Properties)
	remaining := 2 + uint64(len(pub.Topic)) +
		encodedVBILen(properties) + properties +
		uint64(len(pub.Payload))
	if pub.QoS > 0 {
		remaining += 2 // packet identifier
	}
	return 1 + encodedVBILen(remaining) + remaining
}

func encodedPublishPropertiesSize(props *pahov5.PublishProperties) uint64 {
	if props == nil {
		return 0
	}
	var size uint64
	if props.PayloadFormat != nil {
		size += 1 + 1
	}
	if props.MessageExpiry != nil {
		size += 1 + 4
	}
	if props.ContentType != "" {
		size += 1 + 2 + uint64(len(props.ContentType))
	}
	if props.ResponseTopic != "" {
		size += 1 + 2 + uint64(len(props.ResponseTopic))
	}
	if len(props.CorrelationData) > 0 {
		size += 1 + 2 + uint64(len(props.CorrelationData))
	}
	if props.TopicAlias != nil {
		size += 1 + 2
	}
	if props.SubscriptionIdentifier != nil && *props.SubscriptionIdentifier > 0 {
		size += 1 + encodedVBILen(uint64(*props.SubscriptionIdentifier))
	}
	for _, u := range props.User {
		size += 1 + 2 + uint64(len(u.Key)) + 2 + uint64(len(u.Value))
	}
	return size
}

// encodedVBILen returns the width of value encoded as an MQTT variable byte
// integer.
func encodedVBILen(value uint64) uint64 {
	switch {
	case value < 1<<7:
		return 1
	case value < 1<<14:
		return 2
	case value < 1<<21:
		return 3
	default:
		return 4
	}
}

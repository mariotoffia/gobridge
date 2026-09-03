package paho

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/mariotoffia/gobridge/domain/shared"
)

const (
	mqttPublishPacketType = 3
	// mqttPropertyUserProperty is the MQTT v5 User Property identifier.
	mqttPropertyUserProperty = 38
)

var (
	errMQTTVBINonCanonical = errors.New("non-canonical MQTT variable byte integer")
	errMQTTVBITooLong      = errors.New("MQTT variable byte integer exceeds four bytes")
)

type mqttIngressErrorKind uint8

const (
	mqttIngressMalformed mqttIngressErrorKind = iota + 1
	mqttIngressPacketTooLarge
)

// mqttIngressError is the secret-safe typed cause returned by the raw MQTT
// ingress guard. It records only structural byte counts and limits; topic and
// payload contents never enter the error, logs, or lifecycle event.
type mqttIngressError struct {
	kind   mqttIngressErrorKind
	actual uint64
	limit  uint64
	cause  error
}

func (e *mqttIngressError) Error() string {
	if e == nil {
		return "mqtt: ingress packet rejected before decoding"
	}
	switch e.kind {
	case mqttIngressPacketTooLarge:
		return fmt.Sprintf(
			"mqtt: inbound packet size %d exceeds Maximum Packet Size %d before decoding",
			e.actual,
			e.limit,
		)
	default:
		return "mqtt: malformed inbound packet rejected before decoding"
	}
}

func (e *mqttIngressError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// mqttIngressConn frames one inbound MQTT wire packet at a time. It validates
// raw PUBLISH structure before Paho's packets.ReadPacket allocates decoded
// topic, property, and payload representations. Writes and all other net.Conn
// operations delegate unchanged.
type mqttIngressConn struct {
	net.Conn

	readMu            sync.Mutex
	buffer            []byte
	packet            []byte
	packetOffset      int
	readErr           error
	maximumPacketSize uint32
	onViolation       func(error)
	// onTruncate reports a PUBLISH whose User Property list was cut to one
	// entry above the retained cap before decoding, with the count that was
	// on the wire. Nil disables the report; truncation itself is unconditional.
	onTruncate func(count int)
}

func newMQTTIngressConn(
	conn net.Conn,
	maximumPacketSize uint32,
	onViolation func(error),
) *mqttIngressConn {
	return &mqttIngressConn{
		Conn:              conn,
		maximumPacketSize: maximumPacketSize,
		onViolation:       onViolation,
	}
}

func (c *mqttIngressConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	c.readMu.Lock()
	defer c.readMu.Unlock()

	if c.readErr != nil {
		return 0, c.readErr
	}
	if c.packet == nil {
		packet, err := c.readPacket()
		if err != nil {
			return 0, err
		}
		c.packet = packet
		c.packetOffset = 0
	}

	n := copy(p, c.packet[c.packetOffset:])
	c.packetOffset += n
	if c.packetOffset == len(c.packet) {
		c.packet = nil
		c.packetOffset = 0
	}
	return n, nil
}

func (c *mqttIngressConn) readPacket() ([]byte, error) {
	var fixedHeader [1]byte
	if _, err := io.ReadFull(c.Conn, fixedHeader[:]); err != nil {
		return nil, err //nolint:wrapcheck // net.Conn Read must preserve the transport error.
	}
	packetType := fixedHeader[0] >> 4

	var remainingBytes [4]byte
	remainingLength, remainingLengthBytes, err := readMQTTVBI(c.Conn, remainingBytes[:])
	if err != nil {
		if errors.Is(err, errMQTTVBINonCanonical) ||
			errors.Is(err, errMQTTVBITooLong) {
			return nil, c.rejectMalformed(err)
		}
		// A byte fetch failed before the Remaining Length was complete. This is
		// a transport interruption, not proof of malformed MQTT bytes; let Paho
		// close the generation and let autopaho apply reconnect policy.
		return nil, err
	}
	packetSize := uint64(1 + remainingLengthBytes + remainingLength)
	if c.maximumPacketSize > 0 && packetSize > uint64(c.maximumPacketSize) {
		return nil, c.reject(&mqttIngressError{
			kind:   mqttIngressPacketTooLarge,
			actual: packetSize,
			limit:  uint64(c.maximumPacketSize),
			cause:  shared.ErrPayloadTooLarge,
		})
	}

	if cap(c.buffer) < int(packetSize) {
		c.buffer = make([]byte, int(packetSize))
	} else {
		c.buffer = c.buffer[:int(packetSize)]
	}
	packet := c.buffer
	packet[0] = fixedHeader[0]
	copy(packet[1:], remainingBytes[:remainingLengthBytes])
	body := packet[1+remainingLengthBytes:]
	if _, err := io.ReadFull(c.Conn, body); err != nil {
		if packetType == mqttPublishPacketType {
			// Remaining Length advertised more bytes than this connection
			// generation delivered. EOF, timeout, cancellation, and any other
			// underlying read failure remain reconnectable transport errors.
			return nil, fmt.Errorf("read MQTT PUBLISH body: %w", err)
		}
		return nil, err //nolint:wrapcheck // non-PUBLISH packets preserve the transport error.
	}

	if packetType == mqttPublishPacketType {
		layout, err := c.validatePublish(fixedHeader[0], body)
		if err != nil {
			return nil, c.reject(err)
		}
		if layout.userProperties > maxDecodedUserProperties {
			packet, err = truncatePublishUserProperties(
				packet, 1+remainingLengthBytes, layout, maxDecodedUserProperties,
			)
			if err != nil {
				return nil, c.reject(newMQTTMalformedError())
			}
			if c.onTruncate != nil {
				c.onTruncate(layout.userProperties)
			}
		}
	}
	return packet, nil
}

// publishLayout locates the sections of a structurally valid raw PUBLISH body
// (the bytes after the fixed header byte and the Remaining Length digits).
type publishLayout struct {
	// propertiesLengthOffset is where the properties-length variable byte
	// integer starts: after the topic and, for QoS 1/2, the packet identifier.
	propertiesLengthOffset int
	// propertiesOffset is where the first property starts; the payload starts
	// propertiesLength bytes later.
	propertiesOffset int
	propertiesLength int
	userProperties   int
}

func (c *mqttIngressConn) validatePublish(fixedHeader byte, body []byte) (publishLayout, error) {
	var layout publishLayout
	qos := (fixedHeader >> 1) & 0x03
	if qos == 3 {
		return layout, newMQTTMalformedError()
	}

	offset := 0
	topicLength, ok := readMQTTUint16(body, &offset)
	if !ok || topicLength > len(body)-offset {
		return layout, newMQTTMalformedError()
	}
	offset += topicLength

	if qos > 0 {
		packetID, ok := readMQTTUint16(body, &offset)
		if !ok || packetID == 0 {
			return layout, newMQTTMalformedError()
		}
	}
	layout.propertiesLengthOffset = offset

	propertiesLength, propertiesLengthBytes, err := readMQTTVBIFromBytes(body[offset:])
	if err != nil {
		return layout, newMQTTMalformedError()
	}
	offset += propertiesLengthBytes
	if propertiesLength > len(body)-offset {
		return layout, newMQTTMalformedError()
	}
	layout.propertiesOffset = offset
	layout.propertiesLength = propertiesLength

	// Structural validation only. The LOCAL representational caps —
	// max_payload_bytes, the metadata byte cap, and the User Property count
	// cap — are deliberately NOT enforced here: the CONNECT
	// advertises only the whole-packet Maximum Packet Size (max_payload_bytes
	// + the metadata allowance), so a COMPLIANT broker forwards packets that
	// violate any individual local cap while fitting the advertised total.
	// Rejecting such a packet at this level is terminal (there is no way to
	// ack below Paho), and a terminal rejection of a broker-forwardable
	// packet is a publisher-triggerable permanent kill switch: the un-acked
	// packet is redelivered on every clean_start=false resume and re-latches
	// the session forever. Those caps are enforced by the router callback
	// instead (ingressCapViolation), which ACKS-and-DROPS the violation so
	// the broker frees the in-flight slot and never redelivers it.
	//
	// The User Property COUNT is the one cap whose decode cost is not bounded
	// by the wire: Paho materialises every property twice, so the five-byte
	// minimum property turns 128 KiB of metadata into megabytes of decoded
	// structs before the callback can refuse the packet. The caller therefore
	// cuts the list on the raw bytes to one entry above the cap
	// (truncatePublishUserProperties) — the callback still sees a violation
	// and still acks-and-drops it, but the decode never costs more than the
	// retained-slot budget. Every other cap decodes in proportion to the
	// bytes already read into this guard's buffer (≤ the advertised Maximum
	// Packet Size enforced above). This guard's job remains bounding the RAW
	// read (total packet size) and failing closed on malformed structure —
	// both producible only by a broken broker, where terminal is the correct
	// posture.
	properties := body[offset : offset+propertiesLength]
	userProperties, err := validateRawPublishProperties(properties)
	if err != nil {
		return layout, newMQTTMalformedError()
	}
	layout.userProperties = userProperties
	return layout, nil
}

func (c *mqttIngressConn) rejectMalformed(cause error) error {
	return c.reject(&mqttIngressError{
		kind:  mqttIngressMalformed,
		cause: errors.Join(shared.ErrProtocolError, cause),
	})
}

func (c *mqttIngressConn) reject(err error) error {
	if err == nil {
		err = newMQTTMalformedError()
	}
	c.readErr = err
	if c.onViolation != nil {
		c.onViolation(err)
	}
	_ = c.Close()
	return err
}

func newMQTTMalformedError() *mqttIngressError {
	return &mqttIngressError{
		kind:  mqttIngressMalformed,
		cause: shared.ErrProtocolError,
	}
}

func readMQTTVBI(r io.Reader, scratch []byte) (value, width int, err error) {
	for i := 0; i < 4; i++ {
		if _, err = io.ReadFull(r, scratch[i:i+1]); err != nil {
			return 0, 0, fmt.Errorf("read MQTT variable byte integer: %w", err)
		}
		digit := scratch[i]
		value += int(digit&0x7f) << (7 * i)
		width++
		if digit&0x80 == 0 {
			if width > 1 && digit == 0 {
				return 0, 0, errMQTTVBINonCanonical
			}
			return value, width, nil
		}
	}
	return 0, 0, errMQTTVBITooLong
}

// readMQTTVBIFromBytes decodes a variable byte integer at the start of src with
// the same canonical-form and width rules as readMQTTVBI. It allocates nothing:
// the property walk calls it once per property, and a packet at the advertised
// maximum can carry tens of thousands of them.
func readMQTTVBIFromBytes(src []byte) (value, width int, err error) {
	for i := 0; i < 4; i++ {
		if i >= len(src) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		digit := src[i]
		value += int(digit&0x7f) << (7 * i)
		width++
		if digit&0x80 == 0 {
			if width > 1 && digit == 0 {
				return 0, 0, errMQTTVBINonCanonical
			}
			return value, width, nil
		}
	}
	return 0, 0, errMQTTVBITooLong
}

func readMQTTUint16(src []byte, offset *int) (int, bool) {
	if offset == nil || *offset < 0 || len(src)-*offset < 2 {
		return 0, false
	}
	value := int(src[*offset])<<8 | int(src[*offset+1])
	*offset += 2
	return value, true
}

// validateRawPublishProperties walks a raw PUBLISH properties section and
// returns how many User Properties it carries.
func validateRawPublishProperties(properties []byte) (int, error) {
	userProperties := 0
	err := walkRawPublishProperties(properties, func(propertyID, _, _ int) {
		if propertyID == mqttPropertyUserProperty {
			userProperties++
		}
	})
	return userProperties, err
}

// walkRawPublishProperties calls visit once per property in a raw PUBLISH
// properties section with the property identifier and the byte range of the
// whole property, identifier included. It fails on a truncated property and on
// an identifier that is not valid in a PUBLISH.
func walkRawPublishProperties(properties []byte, visit func(propertyID, start, end int)) error {
	offset := 0
	for offset < len(properties) {
		start := offset
		propertyID, width, err := readMQTTVBIFromBytes(properties[offset:])
		if err != nil {
			return err
		}
		offset += width

		switch propertyID {
		case 1: // Payload Format Indicator: byte
			if !skipMQTTBytes(properties, &offset, 1) {
				return io.ErrUnexpectedEOF
			}
		case 2: // Message Expiry Interval: uint32
			if !skipMQTTBytes(properties, &offset, 4) {
				return io.ErrUnexpectedEOF
			}
		case 3, 8, 9: // UTF-8/Binary Data: uint16 length + bytes
			if !skipMQTTLengthPrefixed(properties, &offset) {
				return io.ErrUnexpectedEOF
			}
		case 11: // Subscription Identifier: VBI
			_, propertyWidth, propertyErr := readMQTTVBIFromBytes(properties[offset:])
			if propertyErr != nil {
				return propertyErr
			}
			offset += propertyWidth
		case 35: // Topic Alias: uint16
			if !skipMQTTBytes(properties, &offset, 2) {
				return io.ErrUnexpectedEOF
			}
		case mqttPropertyUserProperty: // User Property: two UTF-8 strings
			if !skipMQTTLengthPrefixed(properties, &offset) {
				return io.ErrUnexpectedEOF
			}
			if !skipMQTTLengthPrefixed(properties, &offset) {
				return io.ErrUnexpectedEOF
			}
		default:
			return errors.New("property identifier is invalid for PUBLISH")
		}
		visit(propertyID, start, offset)
	}
	return nil
}

func skipMQTTLengthPrefixed(src []byte, offset *int) bool {
	length, ok := readMQTTUint16(src, offset)
	return ok && skipMQTTBytes(src, offset, length)
}

func skipMQTTBytes(src []byte, offset *int, count int) bool {
	if offset == nil || count < 0 || *offset < 0 || len(src)-*offset < count {
		return false
	}
	*offset += count
	return true
}

var _ net.Conn = (*mqttIngressConn)(nil)

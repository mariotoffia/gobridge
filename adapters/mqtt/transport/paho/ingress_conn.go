package paho

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/mariotoffia/gobridge/domain/shared"
)

const mqttPublishPacketType = 3

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
		if err := c.validatePublish(fixedHeader[0], body); err != nil {
			return nil, c.reject(err)
		}
	}
	return packet, nil
}

func (c *mqttIngressConn) validatePublish(fixedHeader byte, body []byte) error {
	qos := (fixedHeader >> 1) & 0x03
	if qos == 3 {
		return newMQTTMalformedError()
	}

	offset := 0
	topicLength, ok := readMQTTUint16(body, &offset)
	if !ok || topicLength > len(body)-offset {
		return newMQTTMalformedError()
	}
	offset += topicLength

	if qos > 0 {
		packetID, ok := readMQTTUint16(body, &offset)
		if !ok || packetID == 0 {
			return newMQTTMalformedError()
		}
	}

	propertiesLength, propertiesLengthBytes, err := readMQTTVBIFromBytes(body[offset:])
	if err != nil {
		return newMQTTMalformedError()
	}
	offset += propertiesLengthBytes
	if propertiesLength > len(body)-offset {
		return newMQTTMalformedError()
	}

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
	// the broker frees the in-flight slot and never redelivers it. Decode of
	// an over-cap packet is transient and bounded: the whole packet was
	// already read into this guard's buffer (≤ the advertised Maximum Packet
	// Size enforced above), Paho decodes one packet at a time per connection,
	// and the callback drops the decoded form before anything retains it.
	// This guard's job remains bounding the RAW read (total packet size) and
	// failing closed on malformed structure — both producible only by a
	// broken broker, where terminal is the correct posture.
	properties := body[offset : offset+propertiesLength]
	if _, err := validateRawPublishProperties(properties); err != nil {
		return newMQTTMalformedError()
	}
	return nil
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

func readMQTTVBIFromBytes(src []byte) (value, width int, err error) {
	if len(src) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	reader := &mqttByteReader{src: src}
	var scratch [4]byte
	return readMQTTVBI(reader, scratch[:])
}

type mqttByteReader struct {
	src    []byte
	offset int
}

func (r *mqttByteReader) Read(p []byte) (int, error) {
	if r.offset == len(r.src) {
		return 0, io.EOF
	}
	n := copy(p, r.src[r.offset:])
	r.offset += n
	return n, nil
}

func readMQTTUint16(src []byte, offset *int) (int, bool) {
	if offset == nil || *offset < 0 || len(src)-*offset < 2 {
		return 0, false
	}
	value := int(src[*offset])<<8 | int(src[*offset+1])
	*offset += 2
	return value, true
}

func validateRawPublishProperties(properties []byte) (int, error) {
	offset := 0
	userProperties := 0
	for offset < len(properties) {
		propertyID, width, err := readMQTTVBIFromBytes(properties[offset:])
		if err != nil {
			return 0, err
		}
		offset += width

		switch propertyID {
		case 1: // Payload Format Indicator: byte
			if !skipMQTTBytes(properties, &offset, 1) {
				return 0, io.ErrUnexpectedEOF
			}
		case 2: // Message Expiry Interval: uint32
			if !skipMQTTBytes(properties, &offset, 4) {
				return 0, io.ErrUnexpectedEOF
			}
		case 3, 8, 9: // UTF-8/Binary Data: uint16 length + bytes
			if !skipMQTTLengthPrefixed(properties, &offset) {
				return 0, io.ErrUnexpectedEOF
			}
		case 11: // Subscription Identifier: VBI
			_, propertyWidth, propertyErr := readMQTTVBIFromBytes(properties[offset:])
			if propertyErr != nil {
				return 0, propertyErr
			}
			offset += propertyWidth
		case 35: // Topic Alias: uint16
			if !skipMQTTBytes(properties, &offset, 2) {
				return 0, io.ErrUnexpectedEOF
			}
		case 38: // User Property: two UTF-8 strings
			if !skipMQTTLengthPrefixed(properties, &offset) {
				return 0, io.ErrUnexpectedEOF
			}
			if !skipMQTTLengthPrefixed(properties, &offset) {
				return 0, io.ErrUnexpectedEOF
			}
			userProperties++
		default:
			return 0, errors.New("property identifier is invalid for PUBLISH")
		}
	}
	return userProperties, nil
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

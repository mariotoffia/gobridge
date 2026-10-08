package paho

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"

	"github.com/eclipse/paho.golang/packets"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// mqtt311ProtocolLevel is the CONNECT protocol level of MQTT 3.1.1 (§3.1.2.2).
const mqtt311ProtocolLevel = 4

// mqtt311Conn lets a session speak MQTT 3.1.1 while Paho, the pre-decode guard
// and every session behaviour above them keep speaking MQTT 5 (ADR 0022). It
// wraps the dialled stream, below the guard:
//
//	paho ⇄ mqttIngressConn ⇄ mqtt311Conn ⇄ TCP / TLS / WebSocket
//
// Writes arrive as MQTT 5 and leave as MQTT 3.1.1; reads arrive as MQTT 3.1.1
// and are handed up as MQTT 5. What 3.1.1 cannot carry (properties, reason
// codes, subscription options other than QoS) is dropped on the way out, and
// what it does not send is synthesised on the way in (an empty property block,
// one UNSUBACK Success per filter). An outbound packet that would change
// meaning in translation fails the write and drops the connection instead of
// being weakened.
type mqtt311Conn struct {
	net.Conn

	// maxPayloadBytes is the session's max_payload_bytes. An inbound PUBLISH
	// keeps at most one byte more, so the router's payload cap acks and drops
	// an oversized one instead of the guard dropping the connection.
	maxPayloadBytes uint32
	// maximumPacketSize bounds every other inbound packet: the guard's
	// ceiling, wirePacketSizeFor(max_payload_bytes). Zero disables it.
	maximumPacketSize uint32
	// receiveMaximum is the inbound QoS 1/2 window: the session's
	// receive_maximum, which a 3.1.1 CONNECT cannot announce.
	receiveMaximum int
	onViolation    func(error)

	// pending holds the outbound packet Paho is still writing. Paho writes
	// each packet under the guard's lock, so the bytes of two packets never
	// interleave here.
	pending []byte

	// Read side. Only Paho's reader uses it, serialised by the guard.
	packet  []byte // the MQTT 5 packet being handed up
	offset  int    // how much of packet has been handed up
	discard int64  // payload bytes of the last PUBLISH still on the wire
	readErr error

	// mu guards the state both directions share.
	mu sync.Mutex
	// inflight holds the packet identifiers of inbound QoS 1/2 publishes
	// handed to Paho and not yet released by its PUBACK (QoS 1) or PUBCOMP
	// (QoS 2).
	inflight map[uint16]struct{}
	// pubacked marks, one bit per packet identifier, those Paho's PUBACK
	// released and no inbound publish has reused since.
	pubacked [1 << 16 / 64]uint64
	// unsubscribeFilters is the filter count of each outbound UNSUBSCRIBE: a
	// 3.1.1 UNSUBACK carries no reason codes, and Paho expects one per filter.
	unsubscribeFilters map[uint16]int
}

func newMQTT311Conn(
	conn net.Conn,
	maxPayloadBytes, maximumPacketSize uint32,
	receiveMaximum uint16,
	onViolation func(error),
) *mqtt311Conn {
	window := int(receiveMaximum)
	if window == 0 {
		window = math.MaxUint16 // the MQTT 5 default when none is announced
	}
	return &mqtt311Conn{
		Conn:               conn,
		maxPayloadBytes:    maxPayloadBytes,
		maximumPacketSize:  maximumPacketSize,
		receiveMaximum:     window,
		onViolation:        onViolation,
		inflight:           make(map[uint16]struct{}),
		unsubscribeFilters: make(map[uint16]int),
	}
}

// Write translates each complete MQTT 5 packet in p to MQTT 3.1.1 and writes
// it to the broker. The bytes of a packet not yet complete wait for the next
// Write.
func (c *mqtt311Conn) Write(p []byte) (int, error) {
	c.pending = append(c.pending, p...)
	for {
		size, err := mqttPacketSize(c.pending)
		if err != nil {
			return 0, c.failWrite(err)
		}
		if size == 0 || size > len(c.pending) {
			break
		}
		out, err := c.translateOutbound(c.pending[:size])
		if err != nil {
			return 0, c.failWrite(err)
		}
		c.pending = c.pending[size:]
		if len(out) > 0 {
			if _, err := c.Conn.Write(out); err != nil {
				return 0, err //nolint:wrapcheck // net.Conn Write must preserve the transport error.
			}
		}
	}
	if len(c.pending) == 0 {
		c.pending = nil // do not keep the largest packet's buffer for the life of the connection
	}
	return len(p), nil
}

// failWrite drops the connection on an outbound packet MQTT 3.1.1 cannot
// express. Paho does not drop it on every write error (a QoS 1/2 PUBLISH waits
// for its acknowledgement instead), and the session must not run on as if the
// packet had been sent.
func (c *mqtt311Conn) failWrite(err error) error {
	c.pending = nil
	_ = c.Close()
	return err
}

// mqttPacketSize returns the size of the whole MQTT packet at the start of b,
// or 0 while b does not yet hold its fixed header.
func mqttPacketSize(b []byte) (int, error) {
	if len(b) < 2 {
		return 0, nil
	}
	remaining, width, err := readMQTTVBIFromBytes(b[1:])
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("mqtt 3.1.1: outbound Remaining Length: %w", err)
	}
	return 1 + width + remaining, nil
}

// translateOutbound returns the MQTT 3.1.1 form of one MQTT 5 packet Paho
// wrote, or nil when the packet must not reach the broker. Paho's own codec
// decodes it. Every packet with a 3.1.1 form has the same fixed-header byte in
// both versions (§2.2), so it is kept.
func (c *mqtt311Conn) translateOutbound(v5 []byte) ([]byte, error) {
	cp, err := packets.ReadPacket(bytes.NewReader(v5))
	if err != nil {
		return nil, fmt.Errorf("mqtt 3.1.1: decode outbound packet: %w", err)
	}
	fixedHeader := v5[0]
	switch p := cp.Content.(type) {
	case *packets.Connect:
		return encodeConnect311(p)
	case *packets.Publish:
		return encodePublish311(fixedHeader, p)
	case *packets.Puback, *packets.Pubrec, *packets.Pubrel, *packets.Pubcomp:
		// Reason code and properties have no 3.1.1 form, so a PUBREL that
		// answers an unknown PUBREC (0x92) becomes a plain PUBREL.
		packetID := cp.PacketID()
		if cp.Type == packets.PUBACK || cp.Type == packets.PUBCOMP {
			c.mu.Lock()
			delete(c.inflight, packetID)
			if cp.Type == packets.PUBACK {
				c.pubacked[packetID/64] |= 1 << (packetID % 64)
			}
			c.mu.Unlock()
		}
		return binary.BigEndian.AppendUint16([]byte{fixedHeader, 2}, packetID), nil
	case *packets.Subscribe:
		return encodeSubscribe311(fixedHeader, p)
	case *packets.Unsubscribe:
		c.mu.Lock()
		c.unsubscribeFilters[p.PacketID] = len(p.Topics)
		c.mu.Unlock()
		body := binary.BigEndian.AppendUint16(nil, p.PacketID)
		for _, topic := range p.Topics {
			body = appendMQTTLengthPrefixed(body, topic)
		}
		return frameMQTTPacket(fixedHeader, body), nil
	case *packets.Pingreq:
		return []byte{fixedHeader, 0}, nil
	case *packets.Disconnect:
		// Only a normal disconnection is sent. Any other reason precedes a
		// close, and a 3.1.1 broker publishes the Last Will on a close without
		// DISCONNECT, as an MQTT 5 broker does for every reason but 0x00
		// (MQTT-3.1.2-8). A 3.1.1 DISCONNECT would discard it.
		if p.ReasonCode != packets.DisconnectNormalDisconnection {
			return nil, nil
		}
		return []byte{fixedHeader, 0}, nil
	default:
		return nil, fmt.Errorf("mqtt 3.1.1: %s has no MQTT 3.1.1 form", cp.PacketType())
	}
}

// encodeConnect311 writes a 3.1.1 CONNECT (§3.1). The connect flags keep their
// bit layout. Clean Session follows the session expiry: without one the MQTT 5
// session ends with the connection, which is what Clean Session means, and
// with one it must outlive it. Properties and will properties are dropped.
func encodeConnect311(p *packets.Connect) ([]byte, error) {
	var expiry uint32
	if p.Properties != nil && p.Properties.SessionExpiryInterval != nil {
		expiry = *p.Properties.SessionExpiryInterval
	}
	if p.CleanStart && expiry > 0 {
		return nil, errors.New("mqtt 3.1.1: a clean start that keeps the session has no MQTT 3.1.1 form")
	}
	if p.PasswordFlag && !p.UsernameFlag {
		return nil, errors.New("mqtt 3.1.1: a password without a user name has no MQTT 3.1.1 form (MQTT-3.1.2-22)")
	}
	p.CleanStart = expiry == 0 // Clean Session
	body := appendMQTTLengthPrefixed(nil, "MQTT")
	body = append(body, mqtt311ProtocolLevel, p.PackFlags())
	body = binary.BigEndian.AppendUint16(body, p.KeepAlive)
	body = appendMQTTLengthPrefixed(body, p.ClientID)
	if p.WillFlag {
		body = appendMQTTLengthPrefixed(body, p.WillTopic)
		body = appendMQTTLengthPrefixed(body, p.WillMessage)
	}
	if p.UsernameFlag {
		body = appendMQTTLengthPrefixed(body, p.Username)
	}
	if p.PasswordFlag {
		body = appendMQTTLengthPrefixed(body, p.Password)
	}
	return frameMQTTPacket(packets.CONNECT<<4, body), nil
}

// encodePublish311 writes a 3.1.1 PUBLISH (§3.3): the topic, the packet
// identifier for QoS 1 and 2, and the payload. Properties are dropped. A topic
// alias leaves the topic empty, and 3.1.1 has none.
func encodePublish311(fixedHeader byte, p *packets.Publish) ([]byte, error) {
	if p.Topic == "" {
		return nil, errors.New("mqtt 3.1.1: a PUBLISH by topic alias has no MQTT 3.1.1 form")
	}
	remaining := 2 + len(p.Topic) + len(p.Payload)
	if p.QoS > 0 {
		remaining += 2
	}
	out := appendMQTTVBI(append(make([]byte, 0, 5+remaining), fixedHeader), remaining)
	out = appendMQTTLengthPrefixed(out, p.Topic)
	if p.QoS > 0 {
		out = binary.BigEndian.AppendUint16(out, p.PacketID)
	}
	return append(out, p.Payload...), nil
}

// encodeSubscribe311 writes a 3.1.1 SUBSCRIBE (§3.8) that keeps each filter's
// requested QoS. Retain Handling has no 3.1.1 form and is dropped: every
// SUBSCRIBE replays retained messages. No Local and Retain As Published would
// change what is delivered, so they fail instead.
func encodeSubscribe311(fixedHeader byte, p *packets.Subscribe) ([]byte, error) {
	body := binary.BigEndian.AppendUint16(nil, p.PacketID)
	for _, sub := range p.Subscriptions {
		if sub.NoLocal || sub.RetainAsPublished {
			return nil, errors.New("mqtt 3.1.1: No Local and Retain As Published have no MQTT 3.1.1 form")
		}
		body = appendMQTTLengthPrefixed(body, sub.Topic)
		body = append(body, sub.QoS)
	}
	return frameMQTTPacket(fixedHeader, body), nil
}

// frameMQTTPacket prefixes body with a fixed header.
func frameMQTTPacket(fixedHeader byte, body []byte) []byte {
	out := appendMQTTVBI(append(make([]byte, 0, 5+len(body)), fixedHeader), len(body))
	return append(out, body...)
}

// appendMQTTLengthPrefixed appends b with its two-byte length prefix, the form
// of an MQTT string (§1.5.3) and of the will message and password (§3.1.3).
// Paho's codec caps those at 65,535 bytes, so the prefix cannot overflow.
func appendMQTTLengthPrefixed[T string | []byte](dst []byte, b T) []byte {
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(b)))
	return append(dst, b...)
}

// Read hands up the next inbound packet as MQTT 5, one packet at a time.
func (c *mqtt311Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for c.offset == len(c.packet) {
		if c.readErr != nil {
			return 0, c.readErr
		}
		// Every error is final: a failed read leaves the stream mid-packet.
		c.packet, c.readErr = c.readPacket()
		c.offset = 0
	}
	n := copy(p, c.packet[c.offset:])
	c.offset += n
	return n, nil
}

// readPacket reads one 3.1.1 packet and returns its MQTT 5 form, or nil for a
// retransmission Paho must not see.
func (c *mqtt311Conn) readPacket() ([]byte, error) {
	if c.discard > 0 {
		if _, err := io.CopyN(io.Discard, c.Conn, c.discard); err != nil {
			return nil, fmt.Errorf("mqtt 3.1.1: discard the rest of an oversized PUBLISH: %w", err)
		}
		c.discard = 0
	}
	var fixedHeader [1]byte
	if _, err := io.ReadFull(c.Conn, fixedHeader[:]); err != nil {
		return nil, err //nolint:wrapcheck // net.Conn Read must preserve the transport error.
	}
	var scratch [4]byte
	remaining, width, err := readMQTTVBI(c.Conn, scratch[:])
	if errors.Is(err, errMQTTVBINonCanonical) || errors.Is(err, errMQTTVBITooLong) {
		return nil, c.violation(newMQTTMalformedError())
	}
	if err != nil {
		return nil, err
	}
	if fixedHeader[0]>>4 == packets.PUBLISH {
		return c.readPublish(fixedHeader[0], remaining)
	}
	if size := uint64(1 + width + remaining); c.maximumPacketSize > 0 && size > uint64(c.maximumPacketSize) {
		return nil, c.violation(&mqttIngressError{
			kind:   mqttIngressPacketTooLarge,
			actual: size,
			limit:  uint64(c.maximumPacketSize),
			cause:  shared.ErrPayloadTooLarge,
		})
	}
	body := make([]byte, remaining)
	if _, err := io.ReadFull(c.Conn, body); err != nil {
		return nil, err //nolint:wrapcheck // net.Conn Read must preserve the transport error.
	}
	packet := c.translateInbound(fixedHeader[0], body)
	if packet == nil {
		return nil, c.violation(newMQTTMalformedError())
	}
	return packet, nil
}

// translateInbound checks one packet other than PUBLISH against MQTT 3.1.1
// exactly and returns its MQTT 5 form, or nil when 3.1.1 forbids it. Paho's
// MQTT 5 decoder accepts bytes 3.1.1 forbids, such as a reason code on a
// PUBACK, so this is the only check these packets get. Matching the whole
// fixed-header byte refuses every flag but the fixed ones (§2.2.2).
func (c *mqtt311Conn) translateInbound(fixedHeader byte, body []byte) []byte {
	switch fixedHeader {
	case packets.CONNACK << 4:
		// Reserved flag bits are 0, and Session Present is 0 on a refusal
		// (MQTT-3.2.2-4).
		if len(body) != 2 || body[0] > 1 || (body[0] == 1 && body[1] != 0) {
			return nil
		}
		reason, ok := mqtt5ConnackReason(body[1])
		if !ok {
			return nil
		}
		return []byte{fixedHeader, 3, body[0], reason, 0}
	case packets.PUBACK << 4, packets.PUBREC << 4, packets.PUBREL<<4 | 0x02, packets.PUBCOMP << 4:
		if len(body) != 2 || body[0]|body[1] == 0 {
			return nil
		}
		// Unchanged: MQTT 5 reads Remaining Length 2 as Success without
		// properties (§3.4.2.1).
		return []byte{fixedHeader, 2, body[0], body[1]}
	case packets.SUBACK << 4:
		if len(body) < 3 || body[0]|body[1] == 0 {
			return nil
		}
		for _, code := range body[2:] {
			if code > packets.SubackGrantedQoS2 && code != packets.SubackUnspecifiederror {
				return nil
			}
		}
		// The 3.1.1 return codes are MQTT 5 reason codes with the same meaning.
		return frameMQTTPacket(fixedHeader, append([]byte{body[0], body[1], 0}, body[2:]...))
	case packets.UNSUBACK << 4:
		if len(body) != 2 {
			return nil
		}
		packetID := binary.BigEndian.Uint16(body)
		c.mu.Lock()
		filters, known := c.unsubscribeFilters[packetID]
		delete(c.unsubscribeFilters, packetID)
		c.mu.Unlock()
		if !known {
			return nil
		}
		// The packet identifier, an empty property block and one Success per
		// filter: 3.1.1 has no unsubscribe failure to report.
		v5 := make([]byte, 3+filters)
		copy(v5, body)
		return frameMQTTPacket(fixedHeader, v5)
	case packets.PINGRESP << 4:
		if len(body) != 0 {
			return nil
		}
		return []byte{fixedHeader, 0}
	default:
		// CONNECT, SUBSCRIBE, UNSUBSCRIBE, PINGREQ, DISCONNECT, AUTH and the
		// reserved types: a 3.1.1 server never sends them.
		return nil
	}
}

// mqtt5ConnackReason returns the MQTT 5 reason code with the meaning of a
// 3.1.1 CONNACK return code (§3.2.2.3), so the session classifies a refusal as
// it does on MQTT 5. ok is false for a reserved return code.
func mqtt5ConnackReason(returnCode byte) (reason byte, ok bool) {
	switch returnCode {
	case 0:
		return packets.ConnackSuccess, true
	case 1: // unacceptable protocol version
		return packets.ConnackUnsupportedProtocolVersion, true
	case 2: // identifier rejected
		return packets.ConnackInvalidClientID, true
	case 3: // server unavailable
		return packets.ConnackServerUnavailable, true
	case 4: // bad user name or password
		return packets.ConnackBadUsernameOrPassword, true
	case 5: // not authorized
		return packets.ConnackNotAuthorized, true
	default:
		return 0, false
	}
}

// readPublish translates one inbound PUBLISH (§3.3). It reads no more of the
// payload than one byte past max_payload_bytes: an oversized PUBLISH is handed
// up at once with its payload cut there, so the router acks and drops it in
// receive order, and the rest is discarded at the start of the next read.
// Handing the head up first lets the ack reach the broker while a slow drain
// is still running, and before a PINGRESP queued behind the payload is late.
func (c *mqtt311Conn) readPublish(fixedHeader byte, remaining int) ([]byte, error) {
	qos := (fixedHeader >> 1) & 0x03
	duplicate := fixedHeader&0x08 != 0
	// QoS 3 is reserved (MQTT-3.3.1-4), and QoS 0 has no DUP (MQTT-3.3.1-2).
	if qos == 3 || (qos == 0 && duplicate) || remaining < 2 {
		return nil, c.violation(newMQTTMalformedError())
	}
	var topicLength [2]byte
	if _, err := io.ReadFull(c.Conn, topicLength[:]); err != nil {
		return nil, err //nolint:wrapcheck // net.Conn Read must preserve the transport error.
	}
	topicSize := int(binary.BigEndian.Uint16(topicLength[:]))
	variableHeader := 2 + topicSize // then the packet identifier for QoS 1 and 2
	if qos > 0 {
		variableHeader += 2
	}
	if topicSize == 0 || variableHeader > remaining {
		return nil, c.violation(newMQTTMalformedError())
	}
	payloadSize := remaining - variableHeader
	keep := min(payloadSize, int(c.maxPayloadBytes)+1)

	v5Remaining := variableHeader + 1 + keep // plus the empty property block
	packet := appendMQTTVBI(append(make([]byte, 0, 5+v5Remaining), fixedHeader), v5Remaining)
	packet = append(packet, topicLength[:]...)
	topicStart := len(packet)
	packet = packet[:topicStart+variableHeader-2]
	if _, err := io.ReadFull(c.Conn, packet[topicStart:]); err != nil {
		return nil, err //nolint:wrapcheck // net.Conn Read must preserve the transport error.
	}
	// A topic name holds no wildcard (MQTT-3.3.2-2) and no U+0000 (MQTT-1.5.3-2).
	if bytes.ContainsAny(packet[topicStart:topicStart+topicSize], "+#\x00") {
		return nil, c.violation(newMQTTMalformedError())
	}
	if qos > 0 {
		packetID := binary.BigEndian.Uint16(packet[len(packet)-2:])
		if packetID == 0 {
			return nil, c.violation(newMQTTMalformedError())
		}
		retransmission, err := c.admitInbound(packetID, duplicate)
		if err != nil {
			return nil, c.violation(err)
		}
		if retransmission {
			c.discard = int64(payloadSize)
			return nil, nil
		}
	}
	packet = append(packet, 0) // MQTT 3.1.1 carries no properties
	payloadStart := len(packet)
	packet = packet[:payloadStart+keep]
	if _, err := io.ReadFull(c.Conn, packet[payloadStart:]); err != nil {
		return nil, err //nolint:wrapcheck // net.Conn Read must preserve the transport error.
	}
	c.discard = int64(payloadSize - keep)
	return packet, nil
}

// admitInbound records an inbound QoS 1/2 packet identifier in the in-flight
// set. It reports a retransmission, which Paho must not see, or the error that
// refuses the packet:
//
//   - An identifier still in flight with DUP set is a retransmission. 3.1.1
//     allows one on a live connection and MQTT 5 does not (MQTT-4.4.0-1).
//     Paho acks by identifier, so the late ack of a second copy could release
//     a newer message that reused it. The copy Paho holds settles both.
//   - An identifier Paho's PUBACK released, with DUP set, is a retransmission
//     that crossed the PUBACK, and the broker may already have reused the
//     identifier. A reused identifier's first copy has no DUP (MQTT-4.3.2-1)
//     and on one connection precedes every copy with DUP. Admitting it clears
//     the mark, so only a stale copy is dropped, and a new connection starts
//     unmarked, so a redelivery after a resume is admitted. QoS 2 needs no
//     mark: no copy follows the PUBREL (MQTT-4.3.3-1), so none is read after
//     PUBCOMP releases the identifier.
//   - An identifier still in flight without DUP is a broker reusing one it has
//     not got back.
//   - One identifier more than receive_maximum would block Paho's reader on
//     the router's admission, which closing the socket does not release.
func (c *mqtt311Conn) admitInbound(packetID uint16, duplicate bool) (retransmission bool, err error) {
	word, bit := packetID/64, uint64(1)<<(packetID%64)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, inFlight := c.inflight[packetID]; inFlight {
		if duplicate {
			return true, nil
		}
		return false, newMQTTMalformedError()
	}
	if duplicate && c.pubacked[word]&bit != 0 {
		return true, nil
	}
	if len(c.inflight) >= c.receiveMaximum {
		return false, &mqttIngressError{
			kind:  mqttIngressWindowExceeded,
			limit: uint64(c.receiveMaximum),
			cause: shared.ErrProtocolError,
		}
	}
	c.inflight[packetID] = struct{}{}
	c.pubacked[word] &^= bit
	return false, nil
}

// violation reports a packet the session must not accept and closes the
// socket without a DISCONNECT, so a 3.1.1 broker publishes the Last Will, as
// an MQTT 5 broker does after the guard's reject reason codes.
func (c *mqtt311Conn) violation(err error) error {
	if c.onViolation != nil {
		c.onViolation(err)
	}
	_ = c.Close()
	return err
}

var _ net.Conn = (*mqtt311Conn)(nil)

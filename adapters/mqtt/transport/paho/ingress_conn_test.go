package paho

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMQTTIngressConn_FragmentedAndCoalescedReadsPreservePackets(t *testing.T) {
	first := testPublishPacket(0, "guard/qos0", nil, []byte("first"))
	second := testPublishPacket(1, "guard/qos1", nil, []byte("second"))
	third := testPublishPacket(2, "guard/qos2", nil, []byte("third"))
	wire := append(append(append(append([]byte{}, first...), 0xD0, 0x00), second...), third...)
	underlying := newTestNetConn(wire, 1)
	guarded := newMQTTIngressConn(
		underlying,
		uint32(len(wire)),
		nil,
	)

	var got bytes.Buffer
	scratch := make([]byte, 3)
	for {
		n, err := guarded.Read(scratch)
		got.Write(scratch[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}

	assert.Equal(t, wire, got.Bytes())
	assert.Zero(t, underlying.UnreadBytes())
}

func TestMQTTIngressConn_RemainingLengthVarintBoundariesPass(t *testing.T) {
	for _, remainingLength := range []int{127, 128, 16_383, 16_384} {
		t.Run(testNameForInt(remainingLength), func(t *testing.T) {
			// QoS 0 variable header: two-byte topic length, one-byte topic,
			// and one-byte zero properties length.
			packet := testPublishPacket(
				0,
				"t",
				nil,
				bytes.Repeat([]byte{'p'}, remainingLength-4),
			)
			guarded := newMQTTIngressConn(
				newTestNetConn(packet, 7),
				uint32(len(packet)),
				nil,
			)

			got, err := io.ReadAll(guarded)
			require.NoError(t, err)
			assert.Equal(t, packet, got)
		})
	}
}

func TestMQTTIngressConn_QoSVariableHeadersPass(t *testing.T) {
	for _, qos := range []byte{0, 1, 2} {
		t.Run(testNameForInt(int(qos)), func(t *testing.T) {
			packet := testPublishPacket(qos, "guard/qos", nil, []byte("body"))
			guarded := newMQTTIngressConn(
				newTestNetConn(packet, 2),
				uint32(len(packet)),
				nil,
			)

			got, err := io.ReadAll(guarded)
			require.NoError(t, err)
			assert.Equal(t, packet, got)
		})
	}
}

func TestMQTTIngressConn_NonPublishPacketsPassThrough(t *testing.T) {
	wire := []byte{
		0x20, 0x03, 0x00, 0x00, 0x00, // CONNACK
		0xD0, 0x00, // PINGRESP
		0x40, 0x02, 0x00, 0x01, // PUBACK
	}
	guarded := newMQTTIngressConn(
		newTestNetConn(wire, len(wire)),
		uint32(len(wire)),
		nil,
	)

	got, err := io.ReadAll(guarded)
	require.NoError(t, err)
	assert.Equal(t, wire, got)
}

// TestMQTTIngressConn_RepresentationalCapViolationsPassThrough pins the
// boundary: a packet violating only a LOCAL representational cap
// (oversize payload, too many user properties, oversize metadata) but
// fitting the advertised Maximum Packet Size is FORWARDED by a compliant
// broker, so the raw guard must NOT reject it terminally — there is no way
// to ack below Paho, and an un-acked terminal rejection is a
// publisher-triggerable permanent redelivery/terminal loop. The guard
// passes such packets through; the router callback acks-and-drops them
// (TestRouter_IngressPoisonAckDrop*).
func TestMQTTIngressConn_RepresentationalCapViolationsPassThrough(t *testing.T) {
	tests := []struct {
		name   string
		packet []byte
	}{
		{
			name:   "payload above max_payload_bytes",
			packet: testPublishPacket(1, "guard/oversize", nil, bytes.Repeat([]byte{'p'}, int(DefaultMaxPayloadBytes)+1)),
		},
		{
			name:   "user property count above cap",
			packet: testPublishPacket(1, "guard/properties", testUserProperties(maxIngressUserProperties+1), []byte("ok")),
		},
		{
			name: "topic plus properties metadata above cap",
			packet: testPublishPacket(0,
				string(bytes.Repeat([]byte{'t'}, 65_535)),
				bytes.Repeat([]byte{0x01, 0x00}, 32_765),
				nil,
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poisonCount := 0
			guarded := newMQTTIngressConn(
				newTestNetConn(test.packet, 1<<12),
				uint32(len(test.packet)),
				func(error) { poisonCount++ },
			)

			got, err := io.ReadAll(guarded)
			require.NoError(t, err, "representational cap violations must reach the callback ack-drop, not raw-guard terminal")
			assert.Equal(t, test.packet, got)
			assert.Zero(t, poisonCount)
		})
	}
}

func TestMQTTIngressConn_MalformedPacketsRejectWithTypedCause(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
	}{
		{
			name: "five byte remaining length",
			wire: []byte{0x30, 0x80, 0x80, 0x80, 0x80, 0x00},
		},
		{
			name: "non canonical remaining length",
			wire: []byte{0x30, 0x80, 0x00},
		},
		{
			name: "invalid qos",
			wire: []byte{0x36, 0x00},
		},
		{
			name: "truncated topic length",
			wire: []byte{0x30, 0x01, 0x00},
		},
		{
			name: "topic exceeds packet",
			wire: []byte{0x30, 0x04, 0x00, 0x04, 'a', 'b'},
		},
		{
			name: "qos packet id missing",
			wire: []byte{0x32, 0x04, 0x00, 0x01, 't', 0x00},
		},
		{
			name: "qos packet id zero",
			wire: testPublishPacketWithPacketID(1, "t", 0, nil, nil),
		},
		{
			name: "truncated properties varint",
			wire: []byte{0x30, 0x04, 0x00, 0x01, 't', 0x80},
		},
		{
			name: "properties exceed packet",
			wire: []byte{0x30, 0x05, 0x00, 0x01, 't', 0x02, 0x01},
		},
		{
			name: "invalid publish property identifier",
			wire: testPublishPacket(0, "t", []byte{0x11, 0, 0, 0, 0}, nil),
		},
		{
			name: "truncated user property",
			wire: testPublishPacket(0, "t", []byte{0x26, 0x00, 0x01, 'k'}, nil),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var rejected error
			guarded := newMQTTIngressConn(
				newTestNetConn(test.wire, 1),
				1<<20,
				func(err error) { rejected = err },
			)

			_, err := io.ReadAll(guarded)
			require.Error(t, err)
			assert.Same(t, rejected, err)
			var ingressErr *mqttIngressError
			require.ErrorAs(t, err, &ingressErr)
			assert.Equal(t, mqttIngressMalformed, ingressErr.kind)
		})
	}
}

func TestMQTTIngressConn_PartialFrameTransportErrorsDoNotPoison(t *testing.T) {
	timeoutErr := testIngressTimeoutError{}
	tests := []struct {
		name         string
		conn         net.Conn
		wantIs       error
		wantNetError bool
	}{
		{
			name: "timeout after fixed header",
			conn: newFailAfterNetConn(
				[]byte{0x30},
				1,
				timeoutErr,
			),
			wantNetError: true,
		},
		{
			name:   "EOF during remaining length",
			conn:   newTestNetConn([]byte{0x30}, 1),
			wantIs: io.EOF,
		},
		{
			name: "unexpected EOF during body",
			conn: newTestNetConn(
				[]byte{0x30, 0x05, 0x00, 0x01, 't'},
				1,
			),
			wantIs: io.ErrUnexpectedEOF,
		},
		{
			name: "context cancellation during remaining length",
			conn: newFailAfterNetConn(
				[]byte{0x30},
				1,
				context.Canceled,
			),
			wantIs: context.Canceled,
		},
		{
			name: "deadline during body",
			conn: newFailAfterNetConn(
				[]byte{0x30, 0x05, 0x00},
				3,
				context.DeadlineExceeded,
			),
			wantIs:       context.DeadlineExceeded,
			wantNetError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poisonCount := 0
			guarded := newMQTTIngressConn(
				test.conn,
				1<<20,
				func(error) { poisonCount++ },
			)

			var dst [1]byte
			_, err := guarded.Read(dst[:])
			require.Error(t, err)
			if test.wantIs != nil {
				assert.ErrorIs(t, err, test.wantIs)
			}
			if test.wantNetError {
				var netErr net.Error
				require.ErrorAs(t, err, &netErr)
			}
			var ingressErr *mqttIngressError
			assert.NotErrorAs(t, err, &ingressErr)
			assert.Zero(t, poisonCount)
			assert.NoError(t, guarded.readErr,
				"transport interruption must not latch terminal ingress state")
		})
	}
}

func TestMQTTIngressConn_CompleteMalformedFramesPoisonExactlyOnce(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
	}{
		{
			name: "remaining length has four continuation bytes",
			wire: []byte{0x30, 0x80, 0x80, 0x80, 0x80},
		},
		{
			name: "publish fixed header has invalid qos bits",
			wire: []byte{0x36, 0x00},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poisonCount := 0
			underlying := newTestNetConn(test.wire, 1)
			guarded := newMQTTIngressConn(
				underlying,
				1<<20,
				func(error) { poisonCount++ },
			)

			var dst [1]byte
			_, firstErr := guarded.Read(dst[:])
			require.Error(t, firstErr)
			var ingressErr *mqttIngressError
			require.ErrorAs(t, firstErr, &ingressErr)
			assert.Equal(t, mqttIngressMalformed, ingressErr.kind)

			_, secondErr := guarded.Read(dst[:])
			assert.Same(t, firstErr, secondErr)
			assert.Equal(t, 1, poisonCount)
			assert.Equal(t, 1, underlying.CloseCount())
		})
	}
}

func TestMQTTIngressConn_MaximumPacketSizeRejectsBeforeBodyRead(t *testing.T) {
	packet := testPublishPacket(0, "guard/max-packet", nil, bytes.Repeat([]byte{'p'}, 64))
	underlying := newTestNetConn(packet, len(packet))
	guarded := newMQTTIngressConn(
		underlying,
		uint32(len(packet)-1),
		nil,
	)

	_, err := io.ReadAll(guarded)
	require.Error(t, err)
	var ingressErr *mqttIngressError
	require.ErrorAs(t, err, &ingressErr)
	assert.Equal(t, mqttIngressPacketTooLarge, ingressErr.kind)
	assert.Greater(t, underlying.UnreadBytes(), 0, "body must not be read or allocated after oversized Remaining Length")
}

func TestMQTTIngressConn_DelegatesWritesAddressesAndDeadlines(t *testing.T) {
	underlying := newTestNetConn(nil, 0)
	guarded := newMQTTIngressConn(underlying, 1024, nil)
	deadline := time.Unix(1_700_000_000, 123)

	n, err := guarded.Write([]byte("outbound"))
	require.NoError(t, err)
	assert.Equal(t, len("outbound"), n)
	assert.Equal(t, []byte("outbound"), underlying.Written())
	assert.Equal(t, underlying.LocalAddr(), guarded.LocalAddr())
	assert.Equal(t, underlying.RemoteAddr(), guarded.RemoteAddr())
	require.NoError(t, guarded.SetDeadline(deadline))
	require.NoError(t, guarded.SetReadDeadline(deadline.Add(time.Second)))
	require.NoError(t, guarded.SetWriteDeadline(deadline.Add(2*time.Second)))
	assert.Equal(t, deadline, underlying.Deadline())
	assert.Equal(t, deadline.Add(time.Second), underlying.ReadDeadline())
	assert.Equal(t, deadline.Add(2*time.Second), underlying.WriteDeadline())
	require.NoError(t, guarded.Close())
	assert.Equal(t, 1, underlying.CloseCount())
}

func TestMQTTIngressConn_ContextCancellationAndCloseUnblockRead(t *testing.T) {
	server, client := net.Pipe()
	guarded := newMQTTIngressConn(client, 1024, nil)
	t.Cleanup(func() { _ = server.Close() })
	readStarted := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		close(readStarted)
		var b [1]byte
		_, err := guarded.Read(b[:])
		readDone <- err
	}()
	<-readStarted

	ctx, cancel := context.WithCancel(t.Context())
	stop := context.AfterFunc(ctx, func() { _ = guarded.Close() })
	t.Cleanup(func() { stop() })
	cancel()

	err := <-readDone
	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrClosedPipe)
}

func testPublishPacket(qos byte, topic string, properties, payload []byte) []byte {
	return testPublishPacketWithPacketID(qos, topic, 1, properties, payload)
}

func testPublishPacketWithPacketID(
	qos byte,
	topic string,
	packetID uint16,
	properties, payload []byte,
) []byte {
	var body bytes.Buffer
	body.WriteByte(byte(len(topic) >> 8))
	body.WriteByte(byte(len(topic)))
	body.WriteString(topic)
	if qos > 0 {
		body.WriteByte(byte(packetID >> 8))
		body.WriteByte(byte(packetID))
	}
	body.Write(testEncodeVBI(len(properties)))
	body.Write(properties)
	body.Write(payload)
	return append(
		append([]byte{0x30 | qos<<1}, testEncodeVBI(body.Len())...),
		body.Bytes()...,
	)
}

func testUserProperties(count int) []byte {
	properties := make([]byte, 0, count*5)
	for range count {
		properties = append(properties, 0x26, 0, 0, 0, 0)
	}
	return properties
}

func testEncodeVBI(value int) []byte {
	var out []byte
	for {
		digit := byte(value % 128)
		value /= 128
		if value > 0 {
			digit |= 0x80
		}
		out = append(out, digit)
		if value == 0 {
			return out
		}
	}
}

func testNameForInt(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var reversed [20]byte
	pos := len(reversed)
	for value > 0 {
		pos--
		reversed[pos] = digits[value%10]
		value /= 10
	}
	return string(reversed[pos:])
}

type testNetConn struct {
	mu            sync.Mutex
	reader        *bytes.Reader
	readLimit     int
	written       bytes.Buffer
	local         net.Addr
	remote        net.Addr
	deadline      time.Time
	readDeadline  time.Time
	writeDeadline time.Time
	closeCount    int
}

func newTestNetConn(data []byte, readLimit int) *testNetConn {
	return &testNetConn{
		reader:    bytes.NewReader(data),
		readLimit: readLimit,
		local:     testNetAddr("local"),
		remote:    testNetAddr("remote"),
	}
}

func (c *testNetConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readLimit > 0 && len(p) > c.readLimit {
		p = p[:c.readLimit]
	}
	return c.reader.Read(p)
}

func (c *testNetConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.Write(p)
}

func (c *testNetConn) Close() error {
	c.mu.Lock()
	c.closeCount++
	c.mu.Unlock()
	return nil
}

func (c *testNetConn) LocalAddr() net.Addr  { return c.local }
func (c *testNetConn) RemoteAddr() net.Addr { return c.remote }

func (c *testNetConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *testNetConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	return nil
}

func (c *testNetConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	return nil
}

func (c *testNetConn) UnreadBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reader.Len()
}

func (c *testNetConn) Written() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.written.Bytes()...)
}

func (c *testNetConn) Deadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

func (c *testNetConn) ReadDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readDeadline
}

func (c *testNetConn) WriteDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeDeadline
}

func (c *testNetConn) CloseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCount
}

type testNetAddr string

func (a testNetAddr) Network() string { return "test" }
func (a testNetAddr) String() string  { return string(a) }

type failAfterNetConn struct {
	*testNetConn
	failAfter int
	read      int
	err       error
}

func newFailAfterNetConn(data []byte, failAfter int, err error) *failAfterNetConn {
	return &failAfterNetConn{
		testNetConn: newTestNetConn(data, 1),
		failAfter:   failAfter,
		err:         err,
	}
}

func (c *failAfterNetConn) Read(p []byte) (int, error) {
	if c.read >= c.failAfter {
		return 0, c.err
	}
	if remaining := c.failAfter - c.read; len(p) > remaining {
		p = p[:remaining]
	}
	n, err := c.testNetConn.Read(p)
	c.read += n
	return n, err
}

type testIngressTimeoutError struct{}

func (testIngressTimeoutError) Error() string   { return "ingress read timeout" }
func (testIngressTimeoutError) Timeout() bool   { return true }
func (testIngressTimeoutError) Temporary() bool { return true }

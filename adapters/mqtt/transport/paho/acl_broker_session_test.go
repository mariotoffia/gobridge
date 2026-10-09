package paho

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// brokerSessionEnd is what a loopback broker saw on the one connection a
// session made to end its broker session.
type brokerSessionEnd struct {
	packet []byte // the CONNECT as written on the wire
	body   []byte // the CONNECT after its fixed header
	next   byte   // the first byte of the packet that followed it
}

// serveBrokerSessionEnd plays a broker on a loopback listener for one
// connection: it records the CONNECT, answers with connack, and records the
// first byte of the next packet.
func serveBrokerSessionEnd(t *testing.T, connack []byte) (brokerURL string, seen <-chan brokerSessionEnd) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ends := make(chan brokerSessionEnd, 1)
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var header [1]byte
		var scratch [4]byte
		if _, readErr := io.ReadFull(conn, header[:]); readErr != nil {
			return
		}
		remaining, width, readErr := readMQTTVBI(conn, scratch[:])
		if readErr != nil {
			return
		}
		body := make([]byte, remaining)
		if _, readErr = io.ReadFull(conn, body); readErr != nil {
			return
		}
		if _, writeErr := conn.Write(connack); writeErr != nil {
			return
		}
		var next [1]byte
		if _, readErr = io.ReadFull(conn, next[:]); readErr != nil {
			return
		}
		ends <- brokerSessionEnd{packet: slices.Concat(header[:], scratch[:width], body), body: body, next: next[0]}
		_, _ = io.Copy(io.Discard, conn)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-served
	})
	return "tcp://" + listener.Addr().String(), ends
}

func TestEndBrokerSession_ConnectsWithCleanStartAndNoExpiryThenDisconnects(t *testing.T) {
	isolateProxyEnv(t, nil) // the shell's ALL_PROXY must not route this loopback dial
	brokerURL, seen := serveBrokerSessionEnd(t, []byte{0x20, 0x03, 0x00, 0x00, 0x00})
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       "orders",
		ConnectTimeout: 10 * time.Second,
		Will:           &WillOptions{Topic: "bridge/gone", Payload: "gone"},
	}, connectivity.SessionPersistent, nil)

	require.NoError(t, s.endBrokerSession(boundedDialContext(t)))

	end := wait.RequireReceive(t, seen, 5*time.Second)
	packet, err := packets.ReadPacket(bytes.NewReader(end.packet))
	require.NoError(t, err)
	connect, ok := packet.Content.(*packets.Connect)
	require.True(t, ok)
	assert.Equal(t, "orders", connect.ClientID, "the broker session of the old client ID is the one ended")
	assert.True(t, connect.CleanStart, "clean start discards the session the broker kept")
	require.NotNil(t, connect.Properties)
	require.NotNil(t, connect.Properties.SessionExpiryInterval)
	assert.Zero(t, *connect.Properties.SessionExpiryInterval, "expiry 0 discards the new session on disconnect")
	assert.False(t, connect.WillFlag, "ending a broker session must not register the session's Will")
	wantPacketSize, err := wirePacketSizeFor(s.opts.MaxPayloadBytes)
	require.NoError(t, err)
	require.NotNil(t, connect.Properties.ReceiveMaximum)
	require.NotNil(t, connect.Properties.MaximumPacketSize)
	assert.Equal(t, s.opts.ReceiveMaximum, *connect.Properties.ReceiveMaximum,
		"the CONNECT announces the session's Receive Maximum, as every session connection does")
	assert.Equal(t, wantPacketSize, *connect.Properties.MaximumPacketSize,
		"the CONNECT announces the Maximum Packet Size the ingress guard enforces")
	assert.Equal(t, byte(packets.DISCONNECT<<4), end.next, "the connection ends with a normal DISCONNECT")
}

func TestEndBrokerSession_MQTT311ConnectsWithCleanSession(t *testing.T) {
	isolateProxyEnv(t, nil)
	brokerURL, seen := serveBrokerSessionEnd(t, []byte{0x20, 0x02, 0x00, 0x00})
	s := NewSession(SessionOptions{
		ProtocolVersion: ProtocolVersion311,
		BrokerURLs:      []string{brokerURL},
		ClientID:        "orders",
		ConnectTimeout:  10 * time.Second,
	}, connectivity.SessionPersistent, nil)

	require.NoError(t, s.endBrokerSession(boundedDialContext(t)))

	end := wait.RequireReceive(t, seen, 5*time.Second)
	require.Greater(t, len(end.body), 12)
	assert.Equal(t, byte(mqtt311ProtocolLevel), end.body[6], "protocol level after the name MQTT")
	flags := end.body[7]
	assert.NotZero(t, flags&0x02, "CleanSession=1 discards the session now and on disconnect")
	assert.Zero(t, flags&0x04, "no Will")
	idLength := int(binary.BigEndian.Uint16(end.body[10:12]))
	assert.Equal(t, "orders", string(end.body[12:12+idLength]))
	assert.Equal(t, byte(packets.DISCONNECT<<4), end.next)
}

func TestEndBrokerSession_ReportsARefusedConnection(t *testing.T) {
	isolateProxyEnv(t, nil)
	brokerURL, _ := serveBrokerSessionEnd(t, []byte{0x20, 0x03, 0x00, 0x87, 0x00}) // 0x87 Not authorized
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       "orders",
		ConnectTimeout: 10 * time.Second,
	}, connectivity.SessionPersistent, nil)

	err := s.endBrokerSession(boundedDialContext(t))

	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrNotAuthorized,
		"a refused CONNACK is classified by its reason code, as on every session connection")
}

func TestEndBrokerSession_RefusesPlaintextCredentials(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://127.0.0.1:1"},
		ClientID:       "orders",
		Username:       "bridge",
		ConnectTimeout: 10 * time.Second,
	}, connectivity.SessionPersistent, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.endBrokerSession(ctx)

	require.Error(t, err)
	assert.Equal(t, errPlaintextCredentials(), err)
}

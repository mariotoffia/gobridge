package paho

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestMQTTWebsocketConn_EmptyWriteSendsNoFrame pins that a zero-length Write
// puts nothing on the wire, while every non-empty Write is exactly one frame.
// Paho writes an MQTT packet one buffer at a time, and a SUBSCRIBE or PUBLISH
// without properties carries an empty buffer. Mosquitto 2.1's WebSocket
// listener reads an empty binary frame as garbage and disconnects the client
// for a malformed packet.
func TestMQTTWebsocketConn_EmptyWriteSendsNoFrame(t *testing.T) {
	endpoint, frames := startWebsocketFrameRecorder(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	conn, err := dialMQTTWebsocket(ctx, nil, nil, endpoint)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	for _, empty := range [][]byte{{}, nil} {
		n, writeErr := conn.Write(empty)
		require.NoError(t, writeErr)
		require.Zero(t, n)
	}
	for _, payload := range []string{"first", "second"} {
		n, writeErr := conn.Write([]byte(payload))
		require.NoError(t, writeErr)
		require.Equal(t, len(payload), n)
	}

	// Frames arrive in write order: a frame from an empty write would arrive
	// before "first", and a second frame from one non-empty write would arrive
	// before "second".
	for _, want := range []string{"first", "second"} {
		frame := wait.RequireReceive(t, frames, 5*time.Second)
		require.Equal(t, websocketFrame{fin: true, opcode: websocket.BinaryMessage, payload: want}, frame,
			"each non-empty Write is one final binary frame, and an empty Write is none")
	}
}

// websocketFrame is one frame as it crossed the wire, before any reassembly
// into messages.
type websocketFrame struct {
	fin     bool
	opcode  int
	payload string
}

// startWebsocketFrameRecorder serves a WebSocket endpoint that accepts the
// mqtt subprotocol and reports every frame the client sends. It parses the raw
// frames itself so the check is exact per frame. Gorilla's reader would show an
// empty message as an empty payload, but it joins the frames of a fragmented
// message into one payload, which would hide an empty fragment or a write split
// across frames. The channel closes when the client disconnects or sends a
// frame the recorder cannot parse.
func startWebsocketFrameRecorder(t *testing.T) (*url.URL, <-chan websocketFrame) {
	t.Helper()
	frames := make(chan websocketFrame, 8)
	upgrader := websocket.Upgrader{Subprotocols: []string{"mqtt"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		defer close(frames)
		for {
			frame, readErr := readClientFrame(ws.NetConn())
			if readErr != nil {
				return
			}
			frames <- frame
		}
	}))
	t.Cleanup(server.Close)

	endpoint, err := url.Parse("ws" + strings.TrimPrefix(server.URL, "http"))
	require.NoError(t, err)
	return endpoint, frames
}

// readClientFrame reads one client-to-server frame (RFC 6455 §5.2): the FIN
// bit and opcode, the mask bit and a 7-bit length, the masking key, then the
// payload, which it unmasks. The test sends short frames only, so an extended
// length is reported as an error rather than parsed.
func readClientFrame(r io.Reader) (websocketFrame, error) {
	var header [6]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return websocketFrame{}, err
	}
	size := int(header[1] & 0x7F)
	if header[1]&0x80 == 0 || size > 125 {
		return websocketFrame{}, fmt.Errorf("not a short masked frame: header %x", header[:2])
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return websocketFrame{}, err
	}
	for i := range payload {
		payload[i] ^= header[2+i%4]
	}
	return websocketFrame{fin: header[0]&0x80 != 0, opcode: int(header[0] & 0x0F), payload: string(payload)}, nil
}

package paho

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"
	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

// attemptGuardedConnection establishes the decrypted MQTT byte stream and
// installs the adapter-owned ingress guard before autopaho creates a Paho
// client. Reconnect attempts use the same boundary because autopaho invokes
// AttemptConnection for every connection generation.
func (s *Session) attemptGuardedConnection(
	ctx context.Context,
	cfg autopaho.ClientConfig,
	serverURL *url.URL,
) (net.Conn, error) {
	raw, err := dialMQTTConnection(ctx, cfg, serverURL)
	if err != nil {
		return nil, err
	}

	maximumPacketSize, err := wirePacketSizeFor(s.opts.MaxPayloadBytes)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	guarded := newMQTTIngressConn(
		raw,
		maximumPacketSize,
		s.rejectPredecodeIngress,
	)
	return packets.NewThreadSafeConn(guarded), nil
}

func dialMQTTConnection(
	ctx context.Context,
	cfg autopaho.ClientConfig,
	serverURL *url.URL,
) (net.Conn, error) {
	if serverURL == nil {
		return nil, fmt.Errorf("mqtt: broker URL is nil")
	}
	dialCtx := ctx
	cancel := func() {}
	if cfg.ConnectTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, cfg.ConnectTimeout)
	}
	defer cancel()

	switch strings.ToLower(serverURL.Scheme) {
	case "", "mqtt", "tcp":
		return dialMQTTTCP(dialCtx, serverURL.Host)
	case "ssl", "tls", "mqtts", "mqtt+ssl", "tcps":
		return dialMQTTTLS(dialCtx, cfg.TlsCfg, serverURL.Host)
	case "ws":
		return dialMQTTWebsocket(dialCtx, nil, cfg.WebSocketCfg, serverURL)
	case "wss":
		return dialMQTTWebsocket(dialCtx, cfg.TlsCfg, cfg.WebSocketCfg, serverURL)
	default:
		return nil, fmt.Errorf("mqtt: unsupported broker URL scheme %q", serverURL.Scheme)
	}
}

func dialMQTTTCP(ctx context.Context, address string) (net.Conn, error) {
	if os.Getenv("all_proxy") == "" {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, fmt.Errorf("mqtt: dial TCP broker: %w", err)
		}
		return conn, nil
	}
	conn, err := proxy.Dial(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("mqtt: dial TCP broker through proxy: %w", err)
	}
	return conn, nil
}

func dialMQTTTLS(ctx context.Context, tlsConfig *tls.Config, address string) (net.Conn, error) {
	if os.Getenv("all_proxy") == "" {
		dialer := tls.Dialer{Config: tlsConfig}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, fmt.Errorf("mqtt: dial TLS broker: %w", err)
		}
		return conn, nil
	}

	conn, err := proxy.Dial(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("mqtt: dial TLS broker through proxy: %w", err)
	}
	tlsConn := tls.Client(conn, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mqtt: TLS handshake through proxy: %w", err)
	}
	return tlsConn, nil
}

func dialMQTTWebsocket(
	ctx context.Context,
	tlsConfig *tls.Config,
	cfg *autopaho.WebSocketConfig,
	serverURL *url.URL,
) (net.Conn, error) {
	var dialer *websocket.Dialer
	var header http.Header
	if cfg != nil {
		if cfg.Dialer != nil {
			dialer = cfg.Dialer(serverURL, tlsConfig)
		}
		if cfg.Header != nil {
			header = cfg.Header(serverURL, tlsConfig)
		}
	}
	if dialer == nil {
		copy := *websocket.DefaultDialer
		copy.TLSClientConfig = tlsConfig
		copy.Subprotocols = []string{"mqtt"}
		dialer = &copy
	}
	conn, _, err := dialer.DialContext(ctx, serverURL.String(), header)
	if err != nil {
		return nil, fmt.Errorf("mqtt: websocket connection: %w", err)
	}
	return &mqttWebsocketConn{Conn: conn}, nil
}

// mqttWebsocketConn presents consecutive binary WebSocket messages as the byte
// stream expected by MQTT while preserving net.Conn deadline/address behavior.
type mqttWebsocketConn struct {
	*websocket.Conn
	readMu sync.Mutex
	reader io.Reader
}

func (c *mqttWebsocketConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		if c.reader == nil {
			_, reader, err := c.NextReader()
			if err != nil {
				return 0, err //nolint:wrapcheck // net.Conn Read preserves WebSocket transport errors.
			}
			c.reader = reader
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err //nolint:wrapcheck // net.Conn Read preserves the frame-reader error.
	}
}

func (c *mqttWebsocketConn) Write(p []byte) (int, error) {
	if err := c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err //nolint:wrapcheck // net.Conn Write preserves WebSocket transport errors.
	}
	return len(p), nil
}

func (c *mqttWebsocketConn) SetDeadline(deadline time.Time) error {
	if err := c.SetReadDeadline(deadline); err != nil {
		return err //nolint:wrapcheck // net.Conn deadline methods delegate exactly.
	}
	return c.SetWriteDeadline(deadline) //nolint:wrapcheck // net.Conn deadline methods delegate exactly.
}

var _ net.Conn = (*mqttWebsocketConn)(nil)

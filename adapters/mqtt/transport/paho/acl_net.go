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

	// brokerDialFamily is the single list of supported schemes, shared with
	// durable-identity canonicalization, so a URL that passes preflight is a URL
	// this switch can dial — including its default port when the URL omits one.
	family, defaultPort, _ := brokerDialFamily(serverURL.Scheme)
	address := serverURL.Host
	if serverURL.Port() == "" && serverURL.Hostname() != "" {
		address = net.JoinHostPort(serverURL.Hostname(), defaultPort)
	}
	switch family {
	case "tcp":
		return dialMQTTTCP(dialCtx, address)
	case "ssl":
		return dialMQTTTLS(dialCtx, cfg.TlsCfg, address)
	case "ws":
		return dialMQTTWebsocket(dialCtx, nil, cfg.WebSocketCfg, serverURL)
	case "wss":
		return dialMQTTWebsocket(dialCtx, cfg.TlsCfg, cfg.WebSocketCfg, serverURL)
	default:
		return nil, fmt.Errorf("mqtt: unsupported broker URL scheme %q", serverURL.Scheme)
	}
}

// proxyEnvLookup reads one environment variable. Production passes os.Getenv;
// tests pass a map so a case-sensitivity or NO_PROXY assertion never depends on
// process-wide state.
type proxyEnvLookup func(name string) string

// mqttDirectProxyValue is the explicit opt-out: ALL_PROXY set to "direct" (or
// "direct://") states that the broker is dialed without a proxy. It exists so an
// operator can say so deliberately instead of unsetting a variable other tools
// in the same container depend on.
const mqttDirectProxyValue = "direct"

// brokerProxyDialer resolves the dialer for one broker dial from the
// environment.
//
// golang.org/x/net/proxy.FromEnvironment is not used directly for two reasons.
// It caches the environment in a sync.Once for the lifetime of the process, and
// it falls back to a DIRECT dial when ALL_PROXY is unparseable or names a scheme
// it cannot build. A proxy is a network-control boundary: silently bypassing it
// is the failure this resolver exists to prevent, so an unusable setting fails
// the dial with an actionable error instead.
//
// Both spellings of each variable are read on every dial, uppercase first —
// the same precedence golang.org/x/net/proxy and net/http use, so two resolvers
// in one process can never disagree about which proxy is in force.
//
//nolint:ireturn // proxy.Dialer is a third-party SDK interface; proxy.FromURL and proxy.Direct hand back interfaces, there is no concrete type to return (category 6).
func brokerProxyDialer(lookup proxyEnvLookup) (proxy.Dialer, error) {
	if lookup == nil {
		lookup = os.Getenv
	}
	allProxy := firstNonEmptyEnv(lookup, "ALL_PROXY", "all_proxy")
	if allProxy == "" ||
		strings.EqualFold(allProxy, mqttDirectProxyValue) ||
		strings.EqualFold(allProxy, mqttDirectProxyValue+"://") {
		return proxy.Direct, nil
	}

	proxyURL, err := url.Parse(allProxy)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	if strings.EqualFold(proxyURL.Scheme, mqttDirectProxyValue) {
		return proxy.Direct, nil
	}
	dialer, err := proxy.FromURL(proxyURL, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("build proxy dialer: %w", err)
	}

	noProxy := firstNonEmptyEnv(lookup, "NO_PROXY", "no_proxy")
	if noProxy == "" {
		return dialer, nil
	}
	perHost := proxy.NewPerHost(dialer, proxy.Direct)
	perHost.AddFromString(noProxy)
	return perHost, nil
}

func firstNonEmptyEnv(lookup proxyEnvLookup, names ...string) string {
	for _, name := range names {
		if value := lookup(name); value != "" {
			return value
		}
	}
	return ""
}

// dialBrokerStream opens the TCP byte stream to address, through the resolved
// proxy or directly. It is the single dial seam for every broker scheme so a
// proxy decision cannot differ between plaintext and TLS.
func dialBrokerStream(ctx context.Context, address string) (net.Conn, error) {
	dialer, err := brokerProxyDialer(os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("mqtt: resolve broker proxy: %w", err)
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		// Every dialer this resolver can return (Direct, SOCKS5, PerHost)
		// implements ContextDialer; refusing the rest keeps the connect
		// deadline enforceable rather than dialing without cancellation.
		return nil, fmt.Errorf("mqtt: broker proxy dialer does not honour cancellation")
	}
	conn, err := contextDialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("mqtt: dial TCP broker: %w", err)
	}
	return conn, nil
}

func dialMQTTTCP(ctx context.Context, address string) (net.Conn, error) {
	return dialBrokerStream(ctx, address)
}

func dialMQTTTLS(ctx context.Context, tlsConfig *tls.Config, address string) (net.Conn, error) {
	conn, err := dialBrokerStream(ctx, address)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, brokerTLSConfig(tlsConfig, address))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mqtt: TLS handshake with broker: %w", err)
	}
	return tlsConn, nil
}

// brokerTLSConfig returns the configuration used for the broker handshake,
// guaranteeing a ServerName derived from the broker URL host.
//
// tls.Dialer fills ServerName in from the dial address, but a proxied dial hands
// an already-connected socket to tls.Client, which does not. Without a
// ServerName a certificate-validating ssl:// connection through a proxy has no
// name to verify the broker's certificate against — the handshake either fails
// outright or, where verification is disabled, accepts any certificate. Since
// both paths now go through tls.Client, the name is derived once, here.
//
// The supplied configuration is never mutated: credential rotation swaps in new
// *tls.Config values that dial snapshots and may share across attempts.
func brokerTLSConfig(cfg *tls.Config, address string) *tls.Config {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if cfg == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	}
	if cfg.ServerName != "" || host == "" {
		return cfg
	}
	out := cfg.Clone()
	out.ServerName = host
	return out
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

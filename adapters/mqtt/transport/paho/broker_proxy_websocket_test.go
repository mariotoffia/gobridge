package paho

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A ws:// or wss:// broker is reached through the same proxy decision as every
// other broker scheme: ALL_PROXY routes the TCP connection, a NO_PROXY match or
// ALL_PROXY=direct dials directly, and HTTP_PROXY / HTTPS_PROXY play no part.
//
// Everything runs on loopback. The broker host broker.test never resolves in
// DNS, and the SOCKS5 proxy forwards every CONNECT to the test's listener, so a
// dial that reaches broker.test proves the proxy carried it.
//
// Category: unit (TESTS.md §1).

// TestDialMQTTWebsocket_AllProxyCarriesTheDial pins that ALL_PROXY routes a
// ws:// broker dial, both for the default dialer and for a caller's dialer that
// sets no dial function of its own.
func TestDialMQTTWebsocket_AllProxyCarriesTheDial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller *websocket.Dialer
	}{
		{name: "default_dialer"},
		{name: "caller_dialer_without_a_dial_function", caller: &websocket.Dialer{Subprotocols: []string{"mqtt"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, frames := startWebsocketFrameRecorder(t)
			isolateProxyEnv(t, map[string]string{"ALL_PROXY": "socks5://" + startSOCKS5Proxy(t, endpoint.Host)})

			conn, err := dialMQTTWebsocket(boundedDialContext(t), nil, callerWebsocketConfig(tc.caller),
				&url.URL{Scheme: "ws", Host: "broker.test", Path: "/mqtt"})
			require.NoError(t, err, "only the proxy can reach broker.test")
			t.Cleanup(func() { _ = conn.Close() })

			_, err = conn.Write([]byte("through the proxy"))
			require.NoError(t, err)
			assert.Equal(t, "through the proxy", wait.RequireReceive(t, frames, 5*time.Second).payload)
			if tc.caller != nil {
				assert.Nil(t, tc.caller.NetDialContext, "the caller's dialer is copied, never modified")
			}
		})
	}
}

// TestDialMQTTWebsocket_UnreachableProxyFailsClosed pins that a ws:// dial never
// goes around a proxy it cannot use. The broker listens on loopback and could be
// dialed directly, but the proxy is the network-control boundary.
func TestDialMQTTWebsocket_UnreachableProxyFailsClosed(t *testing.T) {
	endpoint, _ := startWebsocketFrameRecorder(t)
	isolateProxyEnv(t, map[string]string{"ALL_PROXY": "socks5://" + closedLoopbackAddress(t)})

	_, err := dialMQTTWebsocket(boundedDialContext(t), nil, nil, endpoint)
	require.Error(t, err, "a proxy that cannot be reached must fail the dial, not be bypassed")
}

// TestDialMQTTWebsocket_ProxyOptOutDialsDirectly pins both ways out of the proxy
// for a ws:// broker: a NO_PROXY match and ALL_PROXY=direct. The NO_PROXY case
// names a closed proxy, so a dial that used it would fail.
func TestDialMQTTWebsocket_ProxyOptOutDialsDirectly(t *testing.T) {
	closedProxy := "socks5://" + closedLoopbackAddress(t)
	for _, tc := range []struct {
		name     string
		allProxy string
		noProxy  bool
	}{
		{name: "no_proxy_names_the_broker_host", allProxy: closedProxy, noProxy: true},
		{name: "all_proxy_direct", allProxy: "direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := startWebsocketFrameRecorder(t)
			env := map[string]string{"ALL_PROXY": tc.allProxy}
			if tc.noProxy {
				env["NO_PROXY"] = endpoint.Hostname()
			}
			isolateProxyEnv(t, env)

			conn, err := dialMQTTWebsocket(boundedDialContext(t), nil, nil, endpoint)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
		})
	}
}

// TestDialMQTTWebsocket_SecureThroughProxyVerifiesBrokerIdentity is the wss://
// counterpart of TestDialMQTTTLS_ThroughProxyVerifiesBrokerIdentity: ALL_PROXY
// carries the TCP connection, and the TLS handshake on top of it verifies the
// broker certificate against the host in the broker URL.
func TestDialMQTTWebsocket_SecureThroughProxyVerifiesBrokerIdentity(t *testing.T) {
	const brokerHost = "broker.test"

	certificate, pool := brokerTestIdentity(t, brokerHost)
	proxyAddr := startSOCKS5Proxy(t, startSecureWebsocketServer(t, certificate))
	isolateProxyEnv(t, map[string]string{"ALL_PROXY": "socks5://" + proxyAddr})

	conn, err := dialMQTTWebsocket(boundedDialContext(t),
		&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil,
		&url.URL{Scheme: "wss", Host: brokerHost, Path: "/mqtt"})
	require.NoError(t, err, "only the proxy can reach broker.test")
	t.Cleanup(func() { _ = conn.Close() })

	state := conn.(*mqttWebsocketConn).NetConn().(*tls.Conn).ConnectionState()
	assert.Equal(t, brokerHost, state.ServerName,
		"the proxied handshake must verify the broker identity from the broker URL")
	assert.True(t, state.HandshakeComplete)
}

// TestDialMQTTWebsocket_CallerDialerKeepsItsOwnRoute pins that a dialer from
// WebSocketCfg.Dialer keeps the dial function and the proxy function it sets: a
// caller that supplies them has chosen the route. ALL_PROXY names a closed
// proxy, so the dial succeeds only through the caller's own dial function.
func TestDialMQTTWebsocket_CallerDialerKeepsItsOwnRoute(t *testing.T) {
	endpoint, _ := startWebsocketFrameRecorder(t)
	isolateProxyEnv(t, map[string]string{"ALL_PROXY": "socks5://" + closedLoopbackAddress(t)})

	var dialed []string
	proxyAsked := false
	caller := &websocket.Dialer{
		Subprotocols: []string{"mqtt"},
		Proxy: func(*http.Request) (*url.URL, error) {
			proxyAsked = true
			return nil, nil // no proxy for this request
		},
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			var direct net.Dialer
			return direct.DialContext(ctx, network, address)
		},
	}

	conn, err := dialMQTTWebsocket(boundedDialContext(t), nil, callerWebsocketConfig(caller), endpoint)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	assert.Equal(t, []string{endpoint.Host}, dialed, "the caller's dial function opened the connection")
	assert.True(t, proxyAsked, "the caller's proxy function was consulted")
}

// TestBrokerWebsocketDialer_DefaultReadsNoHTTPProxyVariables pins that the
// default WebSocket dialer has no proxy function. gorilla's default one reads
// HTTP_PROXY and HTTPS_PROXY, which are not broker proxy variables. It is
// checked on the dialer because a unit test cannot see it on the wire: net/http
// never proxies a loopback host, and any other host needs DNS.
func TestBrokerWebsocketDialer_DefaultReadsNoHTTPProxyVariables(t *testing.T) {
	dialer := brokerWebsocketDialer(nil, nil, &url.URL{Scheme: "ws", Host: "broker.test"})
	assert.Nil(t, dialer.Proxy, "HTTP_PROXY and HTTPS_PROXY must not route a broker dial")
}

// isolateProxyEnv clears every proxy variable either resolver could read, so the
// developer's shell cannot decide a test, then sets values.
func isolateProxyEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for _, name := range []string{
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
	} {
		t.Setenv(name, values[name])
	}
}

func boundedDialContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// callerWebsocketConfig hands dialer to dialMQTTWebsocket as the caller's own
// WebSocketCfg.Dialer. A nil dialer means no WebSocketCfg at all.
func callerWebsocketConfig(dialer *websocket.Dialer) *autopaho.WebSocketConfig {
	if dialer == nil {
		return nil
	}
	return &autopaho.WebSocketConfig{
		Dialer: func(*url.URL, *tls.Config) *websocket.Dialer { return dialer },
	}
}

// startSecureWebsocketServer serves a TLS WebSocket endpoint that presents
// certificate and accepts the mqtt subprotocol, and returns its listener
// address. Each connection stays open until the client closes it.
func startSecureWebsocketServer(t *testing.T, certificate tls.Certificate) string {
	t.Helper()
	upgrader := websocket.Upgrader{Subprotocols: []string{"mqtt"}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.Listener.Addr().String()
}

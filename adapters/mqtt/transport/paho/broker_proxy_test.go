package paho

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/proxy"

	"github.com/mariotoffia/gobridge/testutil/tlsgen"
)

// TestBrokerProxyDialer_ResolvesBothEnvironmentSpellings pins the proxy
// resolution contract. golang.org/x/net/proxy prefers the UPPERCASE spelling
// and this adapter previously keyed only on lowercase `all_proxy`, so an
// uppercase-only environment dialed the broker directly and silently bypassed
// the network-control boundary the proxy exists to enforce.
func TestBrokerProxyDialer_ResolvesBothEnvironmentSpellings(t *testing.T) {
	env := func(pairs map[string]string) proxyEnvLookup {
		return func(name string) string { return pairs[name] }
	}

	t.Run("uppercase_selects_the_proxy", func(t *testing.T) {
		dialer, err := brokerProxyDialer(env(map[string]string{
			"ALL_PROXY": "socks5://127.0.0.1:1080",
		}))
		require.NoError(t, err)
		require.NotEqual(t, proxy.Dialer(proxy.Direct), dialer)
	})

	t.Run("lowercase_selects_the_proxy", func(t *testing.T) {
		dialer, err := brokerProxyDialer(env(map[string]string{
			"all_proxy": "socks5://127.0.0.1:1080",
		}))
		require.NoError(t, err)
		require.NotEqual(t, proxy.Dialer(proxy.Direct), dialer)
	})

	t.Run("uppercase_wins_over_lowercase", func(t *testing.T) {
		// Matches golang.org/x/net/proxy and net/http: the uppercase spelling
		// is authoritative when both are present, so the two resolvers can
		// never disagree about which proxy is in force.
		lookups := map[string]string{
			"ALL_PROXY": "socks5://127.0.0.1:1080",
			"all_proxy": "socks5://%zz",
		}
		_, err := brokerProxyDialer(env(lookups))
		require.NoError(t, err, "the lowercase value must not be consulted at all")
	})

	t.Run("unset_dials_direct", func(t *testing.T) {
		dialer, err := brokerProxyDialer(env(nil))
		require.NoError(t, err)
		require.Equal(t, proxy.Dialer(proxy.Direct), dialer)
	})

	t.Run("explicit_direct_dials_direct", func(t *testing.T) {
		for _, value := range []string{"direct", "DIRECT", "direct://"} {
			dialer, err := brokerProxyDialer(env(map[string]string{"ALL_PROXY": value}))
			require.NoError(t, err, value)
			require.Equal(t, proxy.Dialer(proxy.Direct), dialer, value)
		}
	})

	t.Run("unusable_value_fails_the_dial_instead_of_bypassing", func(t *testing.T) {
		for _, value := range []string{"://nope", "http://proxy:3128", "socks5://%zz"} {
			_, err := brokerProxyDialer(env(map[string]string{"ALL_PROXY": value}))
			require.Error(t, err,
				"an unusable proxy setting must fail closed, never silently dial direct: %s", value)
		}
	})

	t.Run("no_proxy_exempts_the_broker_host", func(t *testing.T) {
		dialer, err := brokerProxyDialer(env(map[string]string{
			"ALL_PROXY": "socks5://127.0.0.1:1080",
			"NO_PROXY":  "broker.internal",
		}))
		require.NoError(t, err)
		require.IsType(t, &proxy.PerHost{}, dialer)
	})

	t.Run("no_proxy_lowercase_is_honoured", func(t *testing.T) {
		dialer, err := brokerProxyDialer(env(map[string]string{
			"all_proxy": "socks5://127.0.0.1:1080",
			"no_proxy":  "broker.internal",
		}))
		require.NoError(t, err)
		require.IsType(t, &proxy.PerHost{}, dialer)
	})
}

// TestBrokerTLSConfig_DerivesServerNameFromAddress pins the identity a proxied
// TLS handshake validates against. tls.Dialer fills ServerName in from the dial
// address, but a proxied dial hands an already-connected socket to tls.Client,
// which does not — so a certificate-validating ssl:// broker reached through a
// proxy had no name to verify.
func TestBrokerTLSConfig_DerivesServerNameFromAddress(t *testing.T) {
	t.Run("nil_configuration_still_gets_an_identity", func(t *testing.T) {
		got := brokerTLSConfig(nil, "broker.example:8883")
		require.NotNil(t, got)
		require.Equal(t, "broker.example", got.ServerName)
		require.Equal(t, uint16(tls.VersionTLS12), got.MinVersion)
	})

	t.Run("empty_server_name_is_derived_without_mutating_the_source", func(t *testing.T) {
		source := &tls.Config{MinVersion: tls.VersionTLS13}
		got := brokerTLSConfig(source, "broker.example:8883")
		require.Equal(t, "broker.example", got.ServerName)
		require.Empty(t, source.ServerName,
			"credential rotation shares *tls.Config pointers; the source must never be mutated")
	})

	t.Run("explicit_server_name_wins", func(t *testing.T) {
		source := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "pinned.example"}
		got := brokerTLSConfig(source, "broker.example:8883")
		require.Equal(t, "pinned.example", got.ServerName)
	})

	t.Run("address_without_a_port_is_used_verbatim", func(t *testing.T) {
		got := brokerTLSConfig(nil, "broker.example")
		require.Equal(t, "broker.example", got.ServerName)
	})
}

// TestDialMQTTTLS_ThroughProxyVerifiesBrokerIdentity is the end-to-end proof for
// both proxy findings: an UPPERCASE-only ALL_PROXY must route the dial through
// the proxy, and the TLS handshake on the proxied socket must validate the
// broker's certificate against the hostname from the broker URL. It runs
// entirely on loopback with a generated CA — no container, no external network.
func TestDialMQTTTLS_ThroughProxyVerifiesBrokerIdentity(t *testing.T) {
	const brokerHost = "broker.test"

	ca := tlsgen.MustGenerate(tlsgen.Options{
		CommonName: brokerHost,
		DNSNames:   []string{brokerHost},
		ValidFor:   time.Hour,
		IsCA:       true,
	})
	certificate, err := tls.X509KeyPair([]byte(ca.CertPEM), []byte(ca.KeyPEM))
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(ca.CAPEM)))

	brokerAddr := startTLSEchoListener(t, certificate)
	proxyAddr := startSOCKS5Proxy(t, brokerAddr)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	t.Setenv("ALL_PROXY", "socks5://"+proxyAddr)
	t.Setenv("all_proxy", "")

	// The broker host never resolves in DNS; only the SOCKS5 proxy can reach
	// the listener, so a successful handshake proves the proxy was used.
	conn, err := dialMQTTTLS(ctx, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		net.JoinHostPort(brokerHost, "8883"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	state := conn.(*tls.Conn).ConnectionState()
	require.Equal(t, brokerHost, state.ServerName,
		"the proxied handshake must verify the broker identity from the broker URL")
	require.True(t, state.HandshakeComplete)
}

// TestDialMQTTTCP_NoProxyBypassesTheProxy pins that a NO_PROXY match dials the
// target directly: the configured proxy address is closed, so any attempt to
// use it fails.
func TestDialMQTTTCP_NoProxyBypassesTheProxy(t *testing.T) {
	target := startTCPEchoListener(t)
	host, _, err := net.SplitHostPort(target)
	require.NoError(t, err)

	t.Setenv("ALL_PROXY", "socks5://"+closedLoopbackAddress(t))
	t.Setenv("NO_PROXY", host)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	conn, err := dialMQTTTCP(ctx, target)
	require.NoError(t, err, "a NO_PROXY match must dial directly")
	require.NoError(t, conn.Close())
}

// TestDialMQTTTCP_UnusableProxyFailsClosed pins that a broken proxy setting
// stops the dial rather than quietly bypassing network control.
func TestDialMQTTTCP_UnusableProxyFailsClosed(t *testing.T) {
	target := startTCPEchoListener(t)
	t.Setenv("ALL_PROXY", "http://proxy.invalid:3128")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	_, err := dialMQTTTCP(ctx, target)
	require.Error(t, err)
}

func startTCPEchoListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func startTLSEchoListener(t *testing.T, certificate tls.Certificate) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// closedLoopbackAddress returns a loopback address with nothing listening on it.
func closedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

// startSOCKS5Proxy runs a no-authentication SOCKS5 CONNECT proxy that forwards
// every request to target, whatever host the client asked for. That is what
// lets the test dial a name that does not resolve and still reach the listener.
func startSOCKS5Proxy(t *testing.T, target string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveSOCKS5(client, target)
		}
	}()
	return listener.Addr().String()
}

func serveSOCKS5(client net.Conn, target string) {
	defer func() { _ = client.Close() }()

	greeting := make([]byte, 2)
	if _, err := io.ReadFull(client, greeting); err != nil || greeting[0] != 0x05 {
		return
	}
	if _, err := io.ReadFull(client, make([]byte, int(greeting[1]))); err != nil {
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil { // no authentication
		return
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil || header[1] != 0x01 {
		return
	}
	if err := discardSOCKS5Address(client, header[3]); err != nil {
		return
	}

	upstream, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = client.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = upstream.Close() }()

	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(upstream, client) }()
	_, _ = io.Copy(client, upstream)
}

func discardSOCKS5Address(client net.Conn, addressType byte) error {
	var length int
	switch addressType {
	case 0x01: // IPv4
		length = 4
	case 0x03: // domain name
		size := make([]byte, 1)
		if _, err := io.ReadFull(client, size); err != nil {
			return err
		}
		length = int(size[0])
	case 0x04: // IPv6
		length = 16
	default:
		return io.ErrUnexpectedEOF
	}
	// address bytes plus the two-byte port
	_, err := io.ReadFull(client, make([]byte, length+binary.Size(uint16(0))))
	return err
}

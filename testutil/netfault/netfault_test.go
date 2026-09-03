package netfault_test

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/testutil/netfault"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// The proxy is fault-injection infrastructure, so its own faults have to be
// real: a Cut that leaves the connection working, or a Blackhole that still
// forwards, would turn every resilience proof above it into a test of the
// happy path. Each check below drives one fault against a plain echo server
// and requires the observable consequence.
//
// Category: unit (TESTS.md §1) — loopback only, no Docker.

const probeTimeout = 5 * time.Second

func TestProxy_ForwardsBothDirectionsWhenHealthy(t *testing.T) {
	proxy := netfault.Start(t, echoServer(t))

	conn := dial(t, proxy.Addr())
	writeLine(t, conn, "ping")
	require.Equal(t, "ping", readLine(t, conn))
}

func TestProxy_CutClosesLiveConnectionsAndRefusesNewOnes(t *testing.T) {
	proxy := netfault.Start(t, echoServer(t))

	live := dial(t, proxy.Addr())
	writeLine(t, live, "before")
	require.Equal(t, "before", readLine(t, live))

	proxy.Cut()

	require.NoError(t, live.SetReadDeadline(time.Now().Add(probeTimeout)))
	_, err := live.Read(make([]byte, 1))
	require.Error(t, err, "a cut must tear down the connections it already carries")

	requireUnusable(t, proxy.Addr(), "a cut must leave no usable connection to the peer")

	proxy.Heal()
	wait.Until(t, probeTimeout, "proxy carries traffic again", func() bool {
		healed, dialErr := net.DialTimeout("tcp", proxy.Addr(), probeTimeout)
		if dialErr != nil {
			return false
		}
		defer func() { _ = healed.Close() }()
		if _, writeErr := healed.Write([]byte("after\n")); writeErr != nil {
			return false
		}
		_ = healed.SetReadDeadline(time.Now().Add(probeTimeout))
		_, readErr := healed.Read(make([]byte, 16))
		return readErr == nil
	})
}

func TestProxy_BlackholeKeepsTheSocketOpenAndStopsDelivering(t *testing.T) {
	proxy := netfault.Start(t, echoServer(t))

	conn := dial(t, proxy.Addr())
	writeLine(t, conn, "before")
	require.Equal(t, "before", readLine(t, conn))

	// This is what a half-open TCP connection looks like from the client: the
	// socket is established and writable, and nothing ever comes back. It is
	// the failure a keep-alive exists to detect, and the one a plain
	// "connection closed" test never reaches.
	proxy.Blackhole()

	writeLine(t, conn, "swallowed")
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, err := conn.Read(make([]byte, 16))
	var timeout net.Error
	require.True(t, errors.As(err, &timeout) && timeout.Timeout(),
		"a blackholed connection must stall, not fail: got %v", err)
}

func TestProxy_LatencyDelaysDeliveryWithoutLosingBytes(t *testing.T) {
	proxy := netfault.Start(t, echoServer(t))
	proxy.SetLatency(150 * time.Millisecond)

	conn := dial(t, proxy.Addr())
	start := time.Now()
	writeLine(t, conn, "slow")
	got := readLine(t, conn)

	require.Equal(t, "slow", got, "injected latency must delay bytes, never drop them")
	require.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond,
		"the round trip must reflect the injected latency")
}

func TestProxy_RefuseNewConnectionsLeavesLiveOnesAlone(t *testing.T) {
	proxy := netfault.Start(t, echoServer(t))

	live := dial(t, proxy.Addr())
	// A completed round trip is what makes this connection "live": dial returns
	// once the kernel has queued the handshake, which can be before the proxy
	// has accepted it at all — and a connection still sitting in the accept
	// queue is a NEW one as far as RefuseNew is concerned.
	writeLine(t, live, "established")
	require.Equal(t, "established", readLine(t, live))

	proxy.RefuseNew()

	requireUnusable(t, proxy.Addr(), "a refused endpoint must give a new client nothing to talk to")

	// An endpoint that stops accepting is not the same failure as one that
	// drops what it carries — a multi-URL failover test needs to tell them
	// apart.
	writeLine(t, live, "still-here")
	require.Equal(t, "still-here", readLine(t, live))
}

// ---------------------------------------------------------------------------

// requireUnusable pins the client-visible consequence of a refusing proxy. The
// listener stays bound — closing and re-binding it would race another process
// for the port and make the fault injector itself flaky — so a dial completes
// and the connection is reset immediately. To a client that is a transport
// failure either way; what must never happen is a connection that works.
func requireUnusable(t *testing.T, addr, message string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(probeTimeout)))
	if _, err := conn.Write([]byte("probe\n")); err != nil {
		return
	}
	_, err = conn.Read(make([]byte, 16))
	require.Error(t, err, message)
}

func echoServer(t *testing.T) string {
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

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, probeTimeout)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeLine(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(probeTimeout)))
	_, err := conn.Write([]byte(line + "\n"))
	require.NoError(t, err)
}

func readLine(t *testing.T, conn net.Conn) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(probeTimeout)))
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	return string(buf[:n-1])
}

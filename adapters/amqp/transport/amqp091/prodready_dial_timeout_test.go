// ═══════════════════════════════════════════════
// Production-readiness remediation tests: bounded dial (Chunk-11 HIGH-2).
//
// amqp091-go's DialConfig only applies its OWN 30s default when Config.Dial is
// nil. With a ConnectTimeout shorter than 30s, dialWithTimeout gave up at
// ConnectTimeout while the SDK's dial goroutine (TCP connect + TLS + AMQP
// handshake) lived on for the full 30s. Under a SUSTAINED outage every
// reconnect attempt spawned another such goroutine (plus its NotifyClose
// drainer) that outlived the attempt, so they accumulated without bound.
//
// The fix wires Config.Dial = amqp.DefaultDial(dialTimeout(opts)) so the TCP
// connect, TLS handshake, and AMQP handshake all honour the SAME configured
// budget dialWithTimeout races ctx against — the abandoned dial unwinds at
// essentially the same instant, so no attempt starts before the previous is
// torn down.
//
// The behavioural test uses a LOOPBACK listener that accepts TCP but never
// speaks AMQP (deterministic, no external network, ~ConnectTimeout wall clock).
// ═══════════════════════════════════════════════
package amqp091

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestDialTimeout_ResolvesConnectTimeoutWithFloor pins the deadline that gets
// wired into amqp.Config.Dial: a positive ConnectTimeout verbatim, otherwise
// the SDK-default floor (amqp.DefaultDial(0) sets a PAST deadline and
// net.DialTimeout(0) means "no timeout" — either would defeat the bound).
func TestDialTimeout_ResolvesConnectTimeoutWithFloor(t *testing.T) {
	require.Equal(t, 5*time.Second, dialTimeout(SessionOptions{ConnectTimeout: 5 * time.Second}),
		"a positive ConnectTimeout must be used verbatim")
	require.Equal(t, defaultDialTimeout, dialTimeout(SessionOptions{}),
		"an unset ConnectTimeout must floor to the SDK default, not 0 (which breaks DefaultDial)")
	require.Equal(t, defaultDialTimeout, dialTimeout(SessionOptions{ConnectTimeout: -1}),
		"a non-positive ConnectTimeout must floor to the SDK default")
}

// TestDialConfig_WiresBoundedDial pins the regression directly: Config.Dial
// must be set. A nil Dial lets the SDK's fixed 30s default outlive a shorter
// ConnectTimeout, so abandoned dial goroutines accumulate under a sustained
// outage.
//
// Counterfactual (revert the fix): dialConfig returns Config{Dial: nil} and
// this fails.
func TestDialConfig_WiresBoundedDial(t *testing.T) {
	cfg := dialConfig(SessionOptions{ConnectTimeout: time.Second})
	require.NotNil(t, cfg.Dial,
		"dialConfig must set Config.Dial so the handshake honours ConnectTimeout instead of the SDK's fixed 30s")
}

// TestDefaultDial_AcceptedButSilentBroker_BoundedByConnectTimeout is the
// behavioural mutation catcher. A broker that completes the TCP handshake but
// never sends the AMQP preamble makes the SDK block reading the connection
// header. With the fix the dial gives up at ConnectTimeout; without it (or with
// a hard-coded 30s) it blocks for the SDK's default, far past the bound.
//
// Loopback only — no external network, deterministic: the dialer's read
// deadline fires at ConnectTimeout regardless of scheduler timing.
func TestDefaultDial_AcceptedButSilentBroker_BoundedByConnectTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var mu sync.Mutex
	var held []net.Conn
	accepterDone := make(chan struct{})
	go func() {
		defer close(accepterDone)
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed at cleanup
			}
			// Hold the connection open and never send the AMQP preamble, so
			// the dialer blocks reading the connection header until its
			// deadline fires.
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-accepterDone
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	})

	opts := SessionOptions{ConnectTimeout: 100 * time.Millisecond}
	dial := defaultDialFromOpts(opts)
	brokerURL := "amqp://" + ln.Addr().String() + "/"

	res := make(chan error, 1)
	go func() {
		_, derr := dial(brokerURL)
		res <- derr
	}()

	// With the fix the dial fails at ~ConnectTimeout (100ms); without it the
	// SDK's fixed default keeps the goroutine blocked ~30s and this times out.
	err = wait.RequireReceive(t, res, 3*time.Second)
	require.Error(t, err,
		"a broker that accepts TCP but never completes the AMQP handshake must fail the dial "+
			"within ConnectTimeout, not the SDK's fixed 30s")
}

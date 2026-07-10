// ═══════════════════════════════════════════════
// Adversarial-review remediation tests: credential rotation must actually
// DETACH the stale connection on ctx-win and drive the reconnect (review #4).
//
// The pre-fix ApplyCredentials stored the new material and started conn.Close()
// in a goroutine, but s.conn still pointed at the OLD connection and connected
// stayed true until NotifyClose fired. If conn.Close() wedged, reconnect never
// started and senders kept publishing on the stale connection with the OLD
// credentials. The fix atomically drops s.conn / marks the session disconnected
// under the lock and explicitly wakes the reconnect loop (forceReconnect),
// instead of relying on the async close eventually firing NotifyClose.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSession_ApplyCredentials_CtxWin_DetachesStaleConnAndQueuesReconnect pins
// the two halves of the fix in isolation (connection installed directly, no
// reconnect loop): a rotation whose close wedges and whose ctx is already
// cancelled must (1) drop s.conn / mark disconnected / clear activeSubs under
// the lock so the seam never hands out the stale connection, and (2) queue a
// forceReconnect token so the loop redials without waiting for the wedged close.
//
// Mutation A (leave s.conn installed / connected=true): connectionIfReady still
// returns the stale mc → the Nil assertions fail (senders would keep the old
// creds). Mutation B (drop the forceReconnect send): the token is absent → the
// select-default Fatal fires (reconnect would never start on a wedged close).
func TestSession_ApplyCredentials_CtxWin_DetachesStaleConnAndQueuesReconnect(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	mc := newMockConnection()
	mc.CloseFn = func() error {
		<-release // half-dead broker: connection.close-ok never arrives
		return nil
	}

	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	// Install a live, connected connection directly (no reconnect loop) so the
	// test isolates ApplyCredentials's detach + wake behaviour.
	s.mu.Lock()
	s.conn = mc
	s.connected = true
	s.activeSubs = map[string]bool{"q": true}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller's deadline already blown when rotation begins

	err := s.ApplyCredentials(ctx, connectivity.NewCredentialSet(pwCred("u-new", "p-new"), nil))
	require.Error(t, err, "a cancelled rotation returns the mapped ctx error")
	var be *shared.BridgeError
	require.True(t, errors.As(err, &be), "ApplyCredentials must return a classified error, got %v", err)
	require.Equal(t, shared.ErrCodeUnavailable, be.Code,
		"a rotation cancelled mid-close returns ErrUnavailable (mapped context.Canceled)")

	// (1) The stale connection is detached under the lock.
	s.mu.Lock()
	conn := s.conn
	connected := s.connected
	subs := len(s.activeSubs)
	s.mu.Unlock()
	require.Nil(t, conn, "rotation must drop s.conn under the lock")
	require.False(t, connected, "rotation must mark the session disconnected")
	require.Zero(t, subs, "rotation must clear activeSubs for the dropped connection")
	require.Nil(t, s.connectionIfReady(),
		"the seam must not hand out the stale connection after rotation")

	// (2) A forceReconnect token was queued to drive the reconnect.
	select {
	case <-s.forceReconnect:
	default:
		t.Fatal("rotation must queue a forceReconnect to drive the reconnect " +
			"instead of relying on the wedged async close firing NotifyClose")
	}

	// The close was still DISPATCHED (detached), just not awaited.
	wait.Until(t, 2*time.Second, "rotation dispatched the detached close", func() bool {
		return mc.closeCalls() >= 1
	})
}

// TestSession_ReconnectLoop_ForceReconnect_Redials proves the loop half of the
// fix: a forceReconnect token drives a redial even though the current
// connection's NotifyClose never fires (the wedged-close case #4 targets). It
// models what ApplyCredentials does — drop s.conn under the lock, then wake the
// loop — and asserts the loop dials again and installs the new connection.
//
// Mutation (remove the forceReconnect cases from reconnectLoop): the loop stays
// blocked on the old connection's NotifyClose (never fired here), no redial
// happens, and the wait times out.
func TestSession_ReconnectLoop_ForceReconnect_Redials(t *testing.T) {
	mc1 := newMockConnection()
	mc2 := newMockConnection()
	var dialN atomic.Int64
	s := newResilienceSession(func(string) (amqpConnection, error) {
		if dialN.Add(1) == 1 {
			return mc1, nil
		}
		return mc2, nil
	})
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	wait.Until(t, 2*time.Second, "connected on the first connection", func() bool {
		return s.connectionIfReady() == mc1
	})

	// Model ApplyCredentials's detach: drop the stale conn under the lock, then
	// wake the loop with a forceReconnect token (mc1.NotifyClose is never fired).
	s.mu.Lock()
	s.connected = false
	s.conn = nil
	s.mu.Unlock()
	s.forceReconnect <- struct{}{}

	wait.Until(t, 2*time.Second, "forceReconnect drives a redial to the new connection", func() bool {
		return s.connectionIfReady() == mc2
	})
	require.GreaterOrEqual(t, dialN.Load(), int64(2),
		"the forced reconnect must trigger at least a second dial")
}

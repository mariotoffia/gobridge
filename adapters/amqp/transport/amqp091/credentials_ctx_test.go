// ═══════════════════════════════════════════════
// Production-readiness remediation tests: credential rotation honours ctx
// (Chunk-11).
//
// AMQP 0-9-1 has no re-auth primitive, so ApplyCredentials rotates by "close
// then redial": it closes the live connection to let the reconnect loop redial
// with the new material. conn.Close() blocks in the SDK until the broker
// answers connection.close-ok (bounded only by the heartbeat read deadline), so
// on a half-dead broker the old synchronous close pinned the caller — ignoring
// its cancellation/deadline — for the full handshake timeout.
//
// ApplyCredentials now races conn.Close() against ctx (mirroring Session.Close):
// on cancellation it returns promptly with the mapped ctx error while a
// detached goroutine still completes the underlying close, and the rotated
// material is already stored so the redial uses it regardless of who wins.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"

	"github.com/stretchr/testify/require"
)

// TestSession_ApplyCredentials_HonoursContext_HalfDeadBroker is the
// mutation catcher, the credential-rotation analogue of
// TestSession_Close_HonoursContext_HalfDeadBroker: rotating credentials on a
// session whose broker never answers connection.close-ok must not pin the
// caller to that close. With a ctx already cancelled, ApplyCredentials must
// return promptly with ErrUnavailable (mapped context.Canceled), letting the
// detached close finish out-of-band.
//
// The connection is installed directly (no reconnect loop) so the test isolates
// the rotation close-race deterministically — the loop would otherwise redial
// the same mock and spin. Counterfactual (revert to a synchronous conn.Close()):
// ApplyCredentials blocks on the stalled CloseFn forever and RequireReceive
// times out.
func TestSession_ApplyCredentials_HonoursContext_HalfDeadBroker(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // let the detached conn.Close() finish after the test

	mc := newMockConnection()
	mc.CloseFn = func() error {
		<-release // broker never answers connection.close-ok
		return nil
	}
	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	// Install a live connection directly, as a mid-flight rotating session would
	// look, without starting the reconnect loop (which would redial the mock).
	s.mu.Lock()
	s.conn = mc
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller's deadline already blown when rotation begins

	done := make(chan error, 1)
	go func() {
		// A DIFFERENT username/password than the session's zero-value liveCreds,
		// so credsChanged=true and rotation reaches the connection-close path.
		done <- s.ApplyCredentials(ctx, connectivity.NewCredentialSet(pwCred("u-new", "p-new"), nil))
	}()

	err := wait.RequireReceive(t, done, 2*time.Second)
	require.Error(t, err, "a cancelled rotation must not pin the caller to a half-dead broker's conn.Close")
	var be *shared.BridgeError
	require.True(t, errors.As(err, &be), "ApplyCredentials must return a classified error, got %v", err)
	require.Equal(t, shared.ErrCodeUnavailable, be.Code,
		"a rotation cancelled mid-close must return ErrUnavailable (mapped context.Canceled)")

	// The rotated material is still stored under the lock before the close race,
	// so the next dial uses it regardless of who won the race.
	s.mu.Lock()
	gotUser, gotPass := s.liveCreds.Username, s.liveCreds.Password
	s.mu.Unlock()
	require.Equal(t, "u-new", gotUser, "rotated username must be stored even when the close race is lost to ctx")
	require.Equal(t, "p-new", gotPass, "rotated password must be stored even when the close race is lost to ctx")

	// The close was still DISPATCHED (detached), just not awaited: the mock
	// records the call before CloseFn blocks. This proves rotation dropped the
	// stale-credential connection so the reconnect loop redials with the new
	// material — it did not silently skip the close on ctx cancellation.
	wait.Until(t, 2*time.Second, "rotation dispatched the connection close (detached)", func() bool {
		return mc.closeCalls() >= 1
	})
}

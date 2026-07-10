// ═══════════════════════════════════════════════
// Adversarial-review remediation tests: confirm-timeout must not wedge the
// sender mutex (review #1).
//
// When PublishDeferred succeeds but the broker stalls before the publisher
// confirm, pendingConfirm.Wait returns DeadlineExceeded once the batch/send
// deadline fires. The pre-fix Send routed that perr through resetChannelLocked
// WHILE HOLDING s.mu → resetChannelLocked calls sc.Close() → the SDK channel
// close waits for channel.close-ok on the SAME stalled broker → s.mu is wedged
// indefinitely, blocking every future send, shutdown and reconfiguration.
//
// The fix treats a ctx-derived publish/confirm error like the timed-out wedge:
// drop the cached channel under lock, UNLOCK, then abandon/reap the channel
// asynchronously. This test drives that branch through the publisherChannel
// seam with a channel whose Close blocks (closeGate) and asserts a concurrent
// second Send makes progress — impossible if the first Send is still holding
// s.mu behind the wedged Close.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSender_Send_ConfirmTimeout_DoesNotHoldMutex is the review #1 mutation
// catcher. The wedged channel models a broker that accepted the publish but
// stalled before the confirm: PublishConfirmed returns context.DeadlineExceeded
// (exactly what pendingConfirm.Wait yields when the send deadline fires
// mid-confirm), and its Close blocks forever (channel.close-ok never answered).
//
// Under the fix the first Send abandons the channel asynchronously and returns
// promptly, so a concurrent second Send opens a fresh channel and succeeds.
// Mutation (route the ctx-derived perr through resetChannelLocked under s.mu):
// the first Send blocks on the wedged Close while holding s.mu, the second Send
// blocks on s.mu.Lock, and RequireReceive times out.
func TestSender_Send_ConfirmTimeout_DoesNotHoldMutex(t *testing.T) {
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	// closeGate models the SDK channel.Close waiting for channel.close-ok on a
	// half-dead broker. Closed at cleanup so the parked reaper goroutine exits.
	closeGate := make(chan struct{})
	t.Cleanup(func() { close(closeGate) })

	wedged := newWedgeableChannel(func(context.Context) (publishResult, error) {
		return publishResult{}, context.DeadlineExceeded
	})
	wedged.closeGate = closeGate
	healthy := newWedgeableChannel(func(context.Context) (publishResult, error) {
		return publishResult{PublishOK: true, ConfirmedTag: 2}, nil
	})

	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk", Timeout: time.Minute, Clock: clocktest.New()})
	channels := []*wedgeableChannel{wedged, healthy}
	openCalls := 0
	s.openChannel = func(amqpConnection, bool) (publisherChannel, error) {
		ch := channels[openCalls]
		openCalls++
		return ch, nil
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("hi")})
	msg := ports.OutboundMessage{Envelope: env, Address: "rk"}

	// Send #1 hits the confirm-timeout path. Run it off the test goroutine: under
	// the mutation it never returns (wedged Close under s.mu).
	res1 := make(chan error, 1)
	go func() { res1 <- s.Send(context.Background(), msg) }()
	err1 := wait.RequireReceive(t, res1, 2*time.Second)

	require.Error(t, err1, "a confirm-timeout must return an error")
	var be *shared.BridgeError
	require.True(t, errors.As(err1, &be), "the error must be classified, got %v", err1)
	require.Equal(t, shared.ErrorTransient, be.Class,
		"a confirm-timeout must be transient so the runtime retries")

	// The wedged channel must have been dropped under lock so a fresh one opens.
	s.mu.Lock()
	scNil := s.sc == nil
	s.mu.Unlock()
	require.True(t, scNil, "the confirm-stalled channel must be dropped, not cached")

	// Send #2 must make progress on a FRESH channel: proves s.mu was released,
	// not held across the wedged Close.
	res2 := make(chan error, 1)
	go func() { res2 <- s.Send(context.Background(), msg) }()
	err2 := wait.RequireReceive(t, res2, 2*time.Second)
	require.NoError(t, err2, "a subsequent Send must succeed on a fresh channel")
	require.Equal(t, 2, openCalls, "the second Send must open a fresh channel")

	// The confirm-stalled channel stays open until its (blocked) Close is let go.
	require.False(t, wedged.IsClosed(), "the abandoned channel is closed only once its Close unblocks")
}

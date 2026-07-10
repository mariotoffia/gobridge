// ═══════════════════════════════════════════════
// Production-readiness remediation tests: publish-wedge cancellation
// (c5-publish-wedge).
//
// The SDK's PublishWithDeferredConfirmWithContext IGNORES ctx and blocks
// indefinitely while the broker holds connection.blocked flow control — and
// the old Send held the sender mutex across that block, so one wedged publish
// stalled every other publisher plus shutdown/reconfig. Send now races the
// wedgeable publish against the deadline via awaitPublish: on timeout it
// abandons the channel WITHOUT holding the mutex and hands it to a background
// reaper that closes it once the broker finally unblocks.
//
// awaitPublish and reapWedgedChannel are exercised directly (below); the
// Send-level branch that wires them — abandon the channel, release the mutex,
// spawn the reaper, return a transient error — is covered end-to-end via the
// publisherChannel seam (Sender.openChannel), which lets a channel whose
// publish wedges be injected without a live broker.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestAwaitPublish_CompletesBeforeDeadline proves the normal path: a publish
// that returns promptly yields its outcome with timedOut=false and a done
// channel that is already closed (nothing to reap).
func TestAwaitPublish_CompletesBeforeDeadline(t *testing.T) {
	want := publishResult{PublishOK: true, ConfirmedTag: 42}

	out, timedOut, done := awaitPublish(context.Background(), func() (publishResult, error) {
		return want, nil
	})

	require.False(t, timedOut, "a fast publish must not be reported as wedged")
	require.Equal(t, want, out.res)
	require.NoError(t, out.err)
	wait.RequireClosed(t, done, time.Second)
}

// TestAwaitPublish_WedgedPublish_TimesOutThenReapsOnUnblock is the core
// mutation catcher. A publish that ignores ctx and blocks must:
//   - be reported as timedOut=true when ctx is cancelled, and
//   - keep done OPEN until the publish finally unblocks (so the reaper that
//     force-closes the channel is deferred — closing earlier would itself
//     wedge on the SDK send mutex), then close done on unblock.
//
// Mutation: revert Send to a synchronous publish and the wedged goroutine
// never returns to the caller — the whole sender stalls (the exact bug). At
// the helper level, dropping the ctx.Done() select branch makes this test
// hang instead of observing timedOut.
func TestAwaitPublish_WedgedPublish_TimesOutThenReapsOnUnblock(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	publish := func() (publishResult, error) {
		close(started)
		<-release // simulate the SDK publish that ignores ctx and blocks
		return publishResult{PublishOK: true, ConfirmedTag: 7}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the publish is confirmed running, so awaitPublish observes
	// the cancellation while the publish is genuinely wedged.
	go func() {
		<-started
		cancel()
	}()

	out, timedOut, done := awaitPublish(ctx, publish)

	require.True(t, timedOut, "a ctx-ignoring wedged publish must be reported as timed out")
	require.Zero(t, out.res.ConfirmedTag, "the timeout path must not read the still-running publish outcome")

	// Still wedged: done must NOT close yet (reaping is deferred until unblock).
	wait.Silent(t, done, 20*time.Millisecond)

	// Broker unblocks → the publish returns → done closes so the reaper can
	// reclaim the abandoned channel.
	close(release)
	wait.RequireClosed(t, done, time.Second)
}

// TestReapWedgedChannel_ClosesAfterUnblock proves the reaper waits for the
// wedged publish to finish (done closed) before force-closing the channel, and
// then closes it exactly once. Mutation: drop the `<-done` wait and the closer
// runs before done → the "silent before done" assertion fails.
func TestReapWedgedChannel_ClosesAfterUnblock(t *testing.T) {
	done := make(chan struct{})
	fc := &recordingCloser{closed: make(chan struct{})}

	go reapWedgedChannel(done, fc)

	// Before the publish unblocks (done still open) the channel must stay open.
	wait.Silent(t, fc.closed, 20*time.Millisecond)

	// Unblock: the reaper closes the channel.
	close(done)
	wait.RequireClosed(t, fc.closed, time.Second)
}

// recordingCloser is a channelCloser that signals its (single) Close.
type recordingCloser struct{ closed chan struct{} }

func (c *recordingCloser) Close() error { close(c.closed); return nil }

// wedgeableChannel is a publisherChannel test double whose publish behaviour is
// injectable, so the Send-level wedge branch can be driven without a live
// broker (the production *senderChannel wraps a real *amqp.Channel). Close is
// idempotent and observable via closed.
type wedgeableChannel struct {
	publish func(ctx context.Context) (publishResult, error)
	// publishDeferred, when set, backs PublishDeferred so the pipelined
	// SendBatch wedge branch can be driven. Left nil for Send-only tests, which
	// must never touch the deferred path (it panics to catch a mis-wiring).
	publishDeferred func(ctx context.Context) (pendingPublish, error)
	// closeGate, when non-nil, blocks Close until it is closed — modelling the
	// SDK channel.Close that waits for channel.close-ok on a half-dead broker.
	// nil (the default) closes instantly. Used to prove the confirm-timeout
	// path never calls Close under s.mu (review #1).
	closeGate chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

func newWedgeableChannel(publish func(ctx context.Context) (publishResult, error)) *wedgeableChannel {
	return &wedgeableChannel{publish: publish, closed: make(chan struct{})}
}

func (c *wedgeableChannel) PublishConfirmed(
	ctx context.Context, _, _ string, _ bool, _ *messaging.Envelope, _ SenderConfig, _ clock.Clock,
) (publishResult, error) {
	return c.publish(ctx)
}

func (c *wedgeableChannel) PublishDeferred(
	ctx context.Context, _, _ string, _ bool, _ *messaging.Envelope, _ SenderConfig, _ clock.Clock,
) (pendingPublish, error) {
	if c.publishDeferred == nil {
		panic("wedgeableChannel.PublishDeferred: not exercised by this test")
	}
	return c.publishDeferred(ctx)
}

func (c *wedgeableChannel) IsClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *wedgeableChannel) Close() error {
	if c.closeGate != nil {
		<-c.closeGate
	}
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

var _ publisherChannel = (*wedgeableChannel)(nil)

// TestSender_Send_PublishWedge_AbandonsChannelReleasesMutexReturnsTransient is
// the end-to-end mutation catcher for the Send timeout branch (sender.go). It
// injects, via the publisherChannel seam, a first channel whose publish IGNORES
// ctx and blocks (the exact SDK behaviour under connection.blocked) and a second
// that publishes immediately, then asserts the three properties a regression
// would break:
//
//	(a) s.sc is nilled after the timeout — a forgotten `s.sc = nil` would leave
//	    the wedged channel cached and every later publish would reuse it.
//	(b) a SUBSEQUENT Send opens a FRESH channel and returns — this can only
//	    happen if the mutex was released before the reaper was spawned; holding
//	    it across `go reapWedgedChannel` would deadlock Send #2 (test hangs).
//	(c) the returned error is TRANSIENT (ErrUnavailable/ErrTimeout) — mis-mapping
//	    the wedge to a permanent code would make the runtime DLQ a message that
//	    merely hit flow control.
//
// Send's deadline is a CONTEXT deadline (applyTimeout → context.WithTimeout),
// not clock-driven, so the timeout is forced deterministically by cancelling
// ctx while the publish is genuinely wedged — no real sleep. A fake clock is
// injected so the Now()/Since() metric path is deterministic too.
func TestSender_Send_PublishWedge_AbandonsChannelReleasesMutexReturnsTransient(t *testing.T) {
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	release := make(chan struct{})
	started := make(chan struct{})
	wedged := newWedgeableChannel(func(context.Context) (publishResult, error) {
		close(started)
		<-release // ignore ctx, exactly like PublishWithDeferredConfirmWithContext
		return publishResult{PublishOK: true, ConfirmedTag: 1}, nil
	})
	healthy := newWedgeableChannel(func(context.Context) (publishResult, error) {
		return publishResult{PublishOK: true, ConfirmedTag: 2}, nil
	})

	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk", Timeout: time.Minute, Clock: clocktest.New()})
	// openChannel is called under s.mu inside ensureChannelLocked, and the two
	// Sends run sequentially in this goroutine, so a plain counter is race-free.
	channels := []*wedgeableChannel{wedged, healthy}
	openCalls := 0
	s.openChannel = func(amqpConnection, bool) (publisherChannel, error) {
		ch := channels[openCalls]
		openCalls++
		return ch, nil
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("hi")})
	msg := ports.OutboundMessage{Envelope: env, Address: "rk"}

	// Cancel the publish context once the wedged publish is genuinely running,
	// so awaitPublish observes the cancellation mid-wedge.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() {
		<-started
		cancel1()
	}()

	err1 := s.Send(ctx1, msg)

	// (c) transient classification.
	require.Error(t, err1)
	require.True(t, errors.Is(err1, shared.ErrUnavailable) || errors.Is(err1, shared.ErrTimeout),
		"wedge must map to a transient error so the runtime retries, got %v", err1)
	var be *shared.BridgeError
	require.True(t, errors.As(err1, &be))
	require.Equal(t, shared.ErrorTransient, be.Class, "a wedged publish must not be classified permanent")

	// (a) the wedged channel was abandoned.
	s.mu.Lock()
	scNil := s.sc == nil
	s.mu.Unlock()
	require.True(t, scNil, "Send must nil the abandoned channel so a stale wedged channel is never reused")
	require.Equal(t, 1, openCalls, "only the first channel should have been opened so far")

	// (b) a subsequent Send makes progress on a FRESH channel — proving the
	// mutex was released, not held across the reaper spawn (else this deadlocks).
	err2 := s.Send(context.Background(), msg)
	require.NoError(t, err2, "a subsequent publish must succeed on a fresh channel")
	require.Equal(t, 2, openCalls, "the second Send must open a fresh channel")

	// The reaper force-closes the abandoned channel once the broker unblocks.
	require.False(t, wedged.IsClosed(), "the wedged channel must not be closed until the publish unblocks")
	close(release)
	wait.RequireClosed(t, wedged.closed, time.Second)
	require.False(t, healthy.IsClosed(), "the healthy channel must stay open")
}

// TestMapPublishWedge_TransientClassification pins the property that makes the
// runtime retry a wedged publish instead of DLQ-ing it: a cancel maps to
// ErrUnavailable and a deadline to ErrTimeout, both transient. Mutation: return
// a permanent error (e.g. ErrNotFound) and both rows fail.
func TestMapPublishWedge_TransientClassification(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel() // ctx.Err() == context.Canceled

	// A deadline already in the past guarantees ctx.Err() == DeadlineExceeded
	// deterministically, with no sleep.
	expired, cancelExp := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancelExp()

	tests := []struct {
		name string
		ctx  context.Context
		want *shared.BridgeError
	}{
		{"canceled→unavailable", canceled, shared.ErrUnavailable},
		{"deadline→timeout", expired, shared.ErrTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapPublishWedge(tt.ctx)
			require.True(t, errors.Is(err, tt.want), "mapPublishWedge = %v, want %v", err, tt.want)
			var be *shared.BridgeError
			require.True(t, errors.As(err, &be))
			require.Equal(t, shared.ErrorTransient, be.Class, "a wedge must always be transient")
		})
	}
}

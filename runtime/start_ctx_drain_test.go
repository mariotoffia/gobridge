package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// TestStartCtxCancel_InFlightSenderKeepsLiveContextUntilDrain pins the SIGTERM
// contract for EVERY composition root: cancelling the context handed to Start
// must NOT reach the routes. Both shipped binaries cancel their process context
// on SIGTERM and only then ask the supervisor/app to stop the runtime, so if the
// route/receiver/delivery contexts were derived from that same context every
// in-flight send would abort before Stop's "settle accepted deliveries before
// cancelling" phase ever ran — a duplicate (or, under a drop policy, a loss) on
// every rolling restart.
//
// The routes therefore run under a context DETACHED from the caller's, cancelled
// only by Stop. Cancelling the Start context still tears the runtime down — it
// drives Stop under the configured budget — but the in-flight delivery observes
// a LIVE context until the drain releases it.
//
// The discriminator is the same one TestStop_DrainsInFlightBeforeCancel_ByDefault
// uses: the delivery rides the runtime WORK context, and the sender captures
// ctx.Err() at the instant the test releases it. Detached ⇒ nil; derived from the
// caller ⇒ context.Canceled.
func TestStartCtxCancel_InFlightSenderKeepsLiveContextUntilDrain(t *testing.T) {
	fake := clocktest.New()
	sender := newLiveCtxSender()
	t.Cleanup(func() {
		select {
		case <-sender.release:
		default:
			close(sender.release)
		}
	})

	rt := goruntime.New(
		goruntime.WithInstanceID("sigterm-start-ctx"),
		goruntime.WithClock(fake),
		// The budget the builder now derives from bridge.drain_timeout. It is the
		// ceiling the ctx-cancel watcher gives its Stop, so the drain below has
		// room to settle instead of being clamped to the 5s fallback.
		goruntime.WithShutdownTimeout(30*time.Second),
	)
	cfg, _, _ := helperQuiescentRoute("r1", nil)
	recv := newWorkCtxReceiver()
	require.NoError(t, rt.AddRoute(cfg, recv, sender, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, rt.Start(ctx))

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Subject: "t"}))
	go func() { _ = recv.EmitWork(del) }()
	<-sender.entered

	base := settledFakeTimerCount(t, fake)

	// SIGTERM: the process cancels the context it passed to Start. Nothing else.
	cancel()

	// The ctx-cancel watcher must drive Stop, which arms the quiet-window timer of
	// its pre-cancel drain. Reaching base+1 means the teardown is under way, so a
	// runtime that killed its routes on the caller's cancel would already have
	// done so by the time we release below.
	waitFakeTimerAtLeast(t, fake, base+1,
		"cancelling the Start ctx must drive a draining Stop, not a silent teardown")

	close(sender.release)
	if err := <-sender.errAt; err != nil {
		t.Fatalf("in-flight delivery saw a CANCELLED context (ctx.Err()=%v) after the Start ctx was cancelled; "+
			"routes must run detached from the caller's context so the drain budget governs teardown", err)
	}

	// The watcher's Stop still completes: close the quiet window and observe the
	// runtime leave the running state.
	require.Eventually(t, func() bool {
		fake.Advance(60 * time.Millisecond)
		return !rt.IsRunning()
	}, 5*time.Second, 5*time.Millisecond,
		"the ctx-cancel watcher must complete Stop after the drain settles")
}

// TestStop_SecondStopReturnsFirstStopError pins the shutdown truthfulness rule:
// exactly one Stop performs the teardown, and every other Stop reports that
// teardown's outcome. Two callers race on every SIGTERM — the Start-context
// watcher and the supervisor's own stopCurrent — and the loser used to return
// nil, so a composition root that read the loser's result logged a clean stop
// over a drain that had in fact failed.
func TestStop_SecondStopReturnsFirstStopError(t *testing.T) {
	closeErr := errors.New("broker close hung")

	rt := goruntime.New(goruntime.WithInstanceID("stop-error-propagation"))
	cfg, recv, sender := helperQuiescentRoute("r1", nil)
	sess := NewFakeSession()
	sess.CloseErr = closeErr
	// No session config: the session is unmanaged, so Stop closes it directly and
	// its Close error is part of the joined Stop result.
	require.NoError(t, rt.AddRoute(cfg, recv, sender, sess, nil))
	require.NoError(t, rt.Start(context.Background()))

	first := rt.Stop(context.Background())
	require.ErrorIs(t, first, closeErr, "test setup: the first Stop must surface the session close failure")

	second := rt.Stop(context.Background())
	assert.ErrorIs(t, second, closeErr,
		"a second Stop must report the first Stop's error, not a false clean stop")
}

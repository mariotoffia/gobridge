package config

// Finding 1 (config-manager side): a config layer watcher failure must never
// silently shut the bridge down.
//
//   - BOOT: a watcher that fails its FIRST establishment attempt is fatal —
//     Watch returns *WatchStartError so the composition root exits non-zero
//     instead of running blind.
//   - STEADY STATE: a watcher whose change channel closes (the underlying
//     watcher died) must NOT close the manager's output channel. The manager
//     records the degraded state (WatchDegraded/WatchErrors) and re-establishes
//     the layer watcher with capped backoff while the last good config keeps
//     serving.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// reWatcher hands out a fresh change channel per Watch call and can be
// programmed to fail specific establishment attempts. It lets a test simulate
// a watcher dying (close the handed-out channel) and the manager's subsequent
// re-establishment.
type reWatcher struct {
	mu    sync.Mutex
	errs  []error // per-call error; nil (or out of range) = success
	chans []chan *ports.BridgeConfig
	calls int
}

func (w *reWatcher) Watch(_ context.Context) (<-chan *ports.BridgeConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	i := w.calls
	w.calls++
	if i < len(w.errs) && w.errs[i] != nil {
		return nil, w.errs[i]
	}
	ch := make(chan *ports.BridgeConfig, 1)
	w.chans = append(w.chans, ch)
	return ch, nil
}

func (w *reWatcher) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// channel returns the i-th handed-out change channel, or nil when fewer
// establishments have happened.
func (w *reWatcher) channel(i int) chan *ports.BridgeConfig {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i >= len(w.chans) {
		return nil
	}
	return w.chans[i]
}

// TestManager_Watch_BootWatcherFailure_ReturnsWatchStartError validates the
// boot half of Finding 1: a layer watcher that cannot establish at Watch time
// fails the Watch call with a typed *WatchStartError (wrapping the cause) so
// the composition root can exit non-zero, instead of the old behaviour of
// logging Warn and closing the change channel (which drained and stopped the
// whole bridge with exit code 0).
func TestManager_Watch_BootWatcherFailure_ReturnsWatchStartError(t *testing.T) {
	bootErr := errors.New("inotify exhausted")
	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: minimalValidConfig("bridge1")},
		Watcher: &stubWatcher{err: bootErr},
	})

	ch, err := mgr.Watch(context.Background())
	require.Error(t, err, "boot-time watcher failure must fail Watch loudly")
	require.Nil(t, ch)

	var wse *WatchStartError
	require.ErrorAs(t, err, &wse, "error must be a typed *WatchStartError")
	assert.Equal(t, "file", wse.Layer)
	require.ErrorIs(t, err, bootErr, "the watcher's cause must be wrapped")

	// The failed Watch must fully unwind: the manager is not left "running",
	// so a retry is possible and Stop is a safe no-op.
	_, err = mgr.Watch(context.Background())
	require.ErrorAs(t, err, &wse, "a second attempt must hit the watcher again, not errAlreadyRunning")
	mgr.Stop()
}

// TestManager_Watch_SteadyStateWatcherDeath_KeepsChannelOpenAndReestablishes
// validates the steady-state half of Finding 1: when an established layer
// watcher dies (its change channel closes without ctx cancellation), the
// manager must NOT close its output channel. It records the degraded state,
// re-establishes the watcher after backoff, clears the degraded state, and
// resumes emitting merged configs.
func TestManager_Watch_SteadyStateWatcherDeath_KeepsChannelOpenAndReestablishes(t *testing.T) {
	clk := clocktest.New()
	w := &reWatcher{}
	mgr := NewManager(
		Layer{Name: "file", Loader: &stubLoader{cfg: minimalValidConfig("bridge1")}, Watcher: w},
		WithManagerClock(clk),
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	out, err := mgr.Watch(ctx)
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)

	require.False(t, mgr.WatchDegraded(), "healthy watcher must not report degraded")
	require.Equal(t, 1, w.callCount())

	// Kill the established watcher: its change channel closes.
	close(w.channel(0))

	require.Eventually(t, mgr.WatchDegraded, 2*time.Second, 5*time.Millisecond,
		"watcher death must surface via WatchDegraded")
	werrs := mgr.WatchErrors()
	require.Contains(t, werrs, "file")
	require.ErrorIs(t, werrs["file"], errWatchEnded)

	// The output channel must remain OPEN: closing it here is exactly the
	// exit-0 total-outage bug. Nothing was emitted, so a receive must not be
	// ready; a closed channel would make the receive fire with ok == false.
	select {
	case _, ok := <-out:
		require.True(t, ok, "manager output channel must NOT close on a watcher death")
		t.Fatal("no config was emitted; nothing should be readable")
	default:
	}

	// Drive the backoff timer until the manager re-establishes the watcher.
	// Advancing repeatedly is safe: each advance can only fire the pending
	// clk.After timer, and once re-established the supervisor blocks on the
	// new channel.
	require.Eventually(t, func() bool {
		clk.Advance(watchRetryInitial)
		return !mgr.WatchDegraded()
	}, 2*time.Second, 5*time.Millisecond, "manager must re-establish the watcher after backoff")
	require.GreaterOrEqual(t, w.callCount(), 2, "the layer watcher must have been re-established")
	assert.Empty(t, mgr.WatchErrors(), "recovery must clear the recorded watch error")

	// Live reconfiguration is restored: an event on the re-established
	// channel flows through merge+validate to the output channel.
	updated := minimalValidConfig("bridge1")
	updated.Bridge.LogLevel = "debug"
	w.channel(1) <- updated

	select {
	case got, ok := <-out:
		require.True(t, ok)
		assert.Equal(t, "debug", got.Bridge.LogLevel)
	case <-time.After(2 * time.Second):
		t.Fatal("re-established watcher event was not emitted")
	}
}

// TestManager_Watch_ReestablishFailure_RecordsErrorAndRetries validates that a
// FAILED re-establishment attempt (steady state) is not fatal either: the
// error is recorded via WatchErrors and the manager keeps retrying with
// backoff until the watcher comes back.
func TestManager_Watch_ReestablishFailure_RecordsErrorAndRetries(t *testing.T) {
	retryErr := errors.New("watcher still down")
	clk := clocktest.New()
	// Call 0 (boot) succeeds, call 1 (first re-establish) fails, call 2 succeeds.
	w := &reWatcher{errs: []error{nil, retryErr, nil}}
	mgr := NewManager(
		Layer{Name: "file", Loader: &stubLoader{cfg: minimalValidConfig("bridge1")}, Watcher: w},
		WithManagerClock(clk),
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	_, err := mgr.Watch(ctx)
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)

	close(w.channel(0))

	// First retry fails: the establishment error must be observable.
	require.Eventually(t, func() bool {
		clk.Advance(watchRetryMax)
		return errors.Is(mgr.WatchErrors()["file"], retryErr)
	}, 2*time.Second, 5*time.Millisecond, "failed re-establishment must record its error")

	// Next retry succeeds: degraded clears.
	require.Eventually(t, func() bool {
		clk.Advance(watchRetryMax)
		return !mgr.WatchDegraded()
	}, 2*time.Second, 5*time.Millisecond, "manager must keep retrying until the watcher recovers")
	require.GreaterOrEqual(t, w.callCount(), 3)
}

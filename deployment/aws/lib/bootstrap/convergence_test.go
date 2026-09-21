package bootstrap

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// TestReconfig1_ConvergenceDegradedStateSurfaces pins RECONFIG-1's observability:
// once the post-swap convergence watch latches applied-but-not-converged, the
// state is surfaced in the deep-health projection (degradedConfigWatch) AND the
// ConfigDegraded gauge flips to 1 — the same signal the generic Supervisor emits,
// which the shipped bootstrap previously lacked. A later convergence clears both.
func TestReconfig1_ConvergenceDegradedStateSurfaces(t *testing.T) {
	app := NewApp(testBootstrapCfg())
	rec := &ports.RecordingExporter{}
	app.metricsExporter = rec

	// A sentinel runtime for pointer-identity gating (the watch never calls methods
	// on it in this state-machine test).
	rt := &goruntime.Runtime{}
	app.runtimeRef.Set(rt)
	app.convergenceRt = rt

	if degraded, _ := app.degradedConfigWatch(); degraded {
		t.Fatal("a freshly installed runtime must not be degraded before the budget elapses")
	}

	// Budget elapsed without convergence: the watch marks degraded.
	require.True(t, app.markConvergenceDegraded(rt, "config version 7 applied but transport sessions have not converged"))
	degraded, reason := app.degradedConfigWatch()
	require.True(t, degraded, "applied-but-not-converged must surface as degraded")
	require.Contains(t, reason, "not converged")
	require.GreaterOrEqual(t, len(rec.FindEntries(shared.MetricConfigDegraded)), 1,
		"ConfigDegraded gauge must flip to 1 (the signal the shipped process previously lacked)")

	// Sessions converge later: the state clears.
	app.clearConvergenceDegraded(rt)
	degraded2, _ := app.degradedConfigWatch()
	require.False(t, degraded2, "convergence must clear the applied-but-not-converged state")
}

// A reload that says the same thing as the running one keeps the runtime and
// only adopts the new document, so the App reports the new version while the
// watch started by the original swap is still running. The watch's diagnostics
// must name the document the App holds when the diagnostic is written, so an
// operator told "config version N" looks at the version the bridge reports as
// applied. Adopting a document must not move the watch's deadline either: the
// budget belongs to the runtime, and the runtime did not change.
func TestApp_ConvergenceWatch_DiagnosticsNameTheAdoptedDocument(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	app := NewApp(testBootstrapCfg(), WithLogger(convDiscardLogger()))
	app.clk = clk
	app.metricsExporter = &ports.RecordingExporter{}

	// An unstarted runtime reports LevelLive and never converges on its own.
	rt := &goruntime.Runtime{}
	app.runtimeRef.Set(rt)
	app.convergenceRt = rt
	app.appliedRef.Set(&ports.BridgeConfig{Version: 1})

	const ticksToBudget = 10
	const budget = ticksToBudget * bootstrapConvergencePollInterval
	start := clk.Now()

	ctx, cancel := context.WithCancel(t.Context())
	watchDone := make(chan struct{})
	t.Cleanup(func() { cancel(); <-watchDone })
	go func() {
		defer close(watchDone)
		app.runConvergenceWatch(ctx, rt, budget)
	}()
	// The watch reads the clock for its deadline and then arms its poll timer, so
	// an armed timer proves the deadline was taken from the start instant rather
	// than from an already-advanced clock.
	require.Eventually(t, func() bool { return clk.TimerCount() == 1 }, time.Second, time.Millisecond,
		"the convergence watch must arm its poll timer")

	advance := func(ticks int) {
		for range ticks {
			clk.Advance(bootstrapConvergencePollInterval)
		}
	}

	// Half a budget in, an equivalent document is adopted the way a skipped
	// reload adopts one: the applied config moves, the runtime stays.
	advance(ticksToBudget / 2)
	app.appliedRef.Set(&ports.BridgeConfig{Version: 2})

	// Advance to exactly the ORIGINAL expiry and no further. A deadline pushed out
	// by the adoption would leave the watch silent here.
	advance(ticksToBudget / 2)
	require.Eventually(t, func() bool {
		degraded, _ := app.degradedConfigWatch()
		return degraded
	}, 2*time.Second, time.Millisecond,
		"the original budget must still expire on schedule after an equivalent document is adopted")

	degraded, reason := app.degradedConfigWatch()
	require.True(t, degraded)
	assert.Contains(t, reason, "config version 2",
		"the degraded reason must name the document the App holds now")
	assert.NotContains(t, reason, "config version 1",
		"naming the superseded document sends the operator to the wrong version")
	assert.Equal(t, budget, clk.Now().Sub(start),
		"the mark belongs at the original budget expiry, not a budget later")
}

// TestReconfig1_SupersededWatcherCannotMark proves a watcher for a runtime that is
// no longer installed cannot clobber the current state — a new swap owns the
// signal.
func TestReconfig1_SupersededWatcherCannotMark(t *testing.T) {
	app := NewApp(testBootstrapCfg())
	app.metricsExporter = &ports.RecordingExporter{}

	current := &goruntime.Runtime{}
	stale := &goruntime.Runtime{}
	app.runtimeRef.Set(current)
	app.convergenceRt = current

	require.False(t, app.markConvergenceDegraded(stale, "stale"),
		"a superseded watcher must not mark degraded")
	degraded, _ := app.degradedConfigWatch()
	require.False(t, degraded)
}

// TestReconfig1_CancelledParentSkipsWatch pins the shutdown-race guard: when the
// App-lifetime context (rootCtx) is already cancelled — as it is once Stop has run
// while a racing admin commit reaches installPlan — startConvergenceWatch must be
// a no-op (it must NOT call watchWg.Go, which would Add concurrently with Stop's
// watchWg.Wait and panic the shutdown goroutine).
func TestReconfig1_CancelledParentSkipsWatch(t *testing.T) {
	app := NewApp(testBootstrapCfg())
	app.metricsExporter = &ports.RecordingExporter{}
	rt := &goruntime.Runtime{}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // rootCtx already cancelled by Stop

	app.startConvergenceWatch(cancelled, rt, &ports.BridgeConfig{})

	if app.convergenceWatchCancel != nil {
		t.Fatal("startConvergenceWatch registered a watch under a cancelled parent; it must skip to avoid a watchWg Add/Wait panic during shutdown")
	}
	// watchWg must have no pending goroutine from this call; Wait returns immediately.
	app.watchWg.Wait()
}

// TestReconfig1_ConvergenceBudgetFloor proves the budget defaults to the floor
// when no session declares transport activation timing.
func TestReconfig1_ConvergenceBudgetFloor(t *testing.T) {
	app := NewApp(testBootstrapCfg())
	require.Equal(t, bootstrapConvergenceBudgetFloor, app.convergenceBudget(&ports.BridgeConfig{}))
}

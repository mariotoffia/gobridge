package bootstrap

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// newConvergenceTestApp is newInPlaceTestApp on a fake clock, with the
// App-lifetime context convergence watches run under.
func newConvergenceTestApp(t *testing.T) (*App, *clocktest.Fake) {
	t.Helper()
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	app := newInPlaceTestApp(t, newTrackedTransportFactory(false), adminKeyResolver())
	app.clk = clk
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { cancel(); app.watchWg.Wait() })
	app.rootCtx = ctx
	return app, clk
}

// The runtime a reload kept runs a configuration its convergence watch has not
// judged yet, so the reload starts a fresh one, as a swap does for the runtime
// it installs.
func TestApplyInPlace_RestartsTheConvergenceWatch(t *testing.T) {
	app, _ := newConvergenceTestApp(t)
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	installs := 0
	app.onRuntimeInstalled = func() { installs++ }
	require.True(t, app.markConvergenceDegraded(app.CurrentRuntime(), app.convergenceGeneration(),
		"the running configuration has not converged"))

	require.NoError(t, applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2)))

	degraded, _ := app.convergenceDegradedState()
	assert.False(t, degraded, "the reloaded configuration gets a convergence attempt of its own")
	assert.Equal(t, 1, installs)
}

// Both watches judge the same runtime, so only the generation tells the watch
// of the configuration it ran before from the watch of the one it runs now.
// The older watch may mark nothing, and nothing is marked before the newer
// watch's budget expires. The fake sessions never report healthy, so the
// runtime never converges.
func TestApplyInPlace_ConvergenceWatchOfOlderGenerationCannotMark(t *testing.T) {
	app, clk := newConvergenceTestApp(t)
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt := app.CurrentRuntime()
	wait.Until(t, time.Second, "the watch arms its poll timer", func() bool { return clk.TimerCount() == 1 })
	olderGen := app.convergenceGeneration()
	// Half the older watch's budget passes first, so its deadline falls well
	// inside the newer watch's budget.
	clk.Advance(bootstrapConvergenceBudgetFloor / 2)

	reloadAt := clk.Now()
	require.NoError(t, applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2)))
	require.Same(t, rt, app.CurrentRuntime())

	assert.False(t, app.markConvergenceDegraded(rt, olderGen, "stale"),
		"the watch of the configuration the runtime ran before cannot mark it")
	assert.False(t, app.convergenceWatcherCurrent(rt, olderGen), "the older watch stops at its next poll")
	// Each step fires the poll timer of whichever watch still runs; the older
	// watch's deadline passes on the second.
	wait.Until(t, 5*time.Second, "the newer watch marks the runtime when its budget expires", func() bool {
		clk.Advance(bootstrapConvergenceBudgetFloor / 3)
		degraded, _ := app.convergenceDegradedState()
		return degraded
	})
	assert.GreaterOrEqual(t, clk.Now().Sub(reloadAt), bootstrapConvergenceBudgetFloor,
		"no watch marks the runtime before the newer watch's budget expires")
	_, reason := app.convergenceDegradedState()
	assert.Contains(t, reason, "config version 2")
}

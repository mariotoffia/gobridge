package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

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
	app.clearConvergenceDegraded(rt, 7)
	degraded2, _ := app.degradedConfigWatch()
	require.False(t, degraded2, "convergence must clear the applied-but-not-converged state")
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

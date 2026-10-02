package bootstrap

import (
	"context"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestApp_ConvergenceDegradedStateSurfaces pins reconfiguration observability:
// once the post-swap convergence watch latches applied-but-not-converged, the
// state is surfaced in the deep-health projection (degradedConfigWatch) AND the
// ConfigDegraded gauge flips to 1 — the same signal the generic Supervisor emits,
// which the shipped bootstrap previously lacked. A later convergence clears both.
func TestApp_ConvergenceDegradedStateSurfaces(t *testing.T) {
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
	require.True(t, app.markConvergenceDegraded(rt, app.convergenceGeneration(), "config version 7 applied but transport sessions have not converged"))
	degraded, reason := app.degradedConfigWatch()
	require.True(t, degraded, "applied-but-not-converged must surface as degraded")
	require.Contains(t, reason, "not converged")
	require.GreaterOrEqual(t, len(rec.FindEntries(shared.MetricConfigDegraded)), 1,
		"ConfigDegraded gauge must flip to 1 (the signal the shipped process previously lacked)")

	// Sessions converge later: the state clears.
	app.clearConvergenceDegraded(rt, app.convergenceGeneration())
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
		app.runConvergenceWatch(ctx, rt, app.convergenceGeneration(), budget)
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

// TestApp_ConvergenceWatch_SupersededWatcherCannotMark proves a watcher for a runtime that is
// no longer installed cannot clobber the current state — a new swap owns the
// signal.
func TestApp_ConvergenceWatch_SupersededWatcherCannotMark(t *testing.T) {
	app := NewApp(testBootstrapCfg())
	app.metricsExporter = &ports.RecordingExporter{}

	current := &goruntime.Runtime{}
	stale := &goruntime.Runtime{}
	app.runtimeRef.Set(current)
	app.convergenceRt = current

	require.False(t, app.markConvergenceDegraded(stale, app.convergenceGeneration(), "stale"),
		"a superseded watcher must not mark degraded")
	degraded, _ := app.degradedConfigWatch()
	require.False(t, degraded)
}

// installPlan publishes a runtime before it starts that runtime's watch, so for
// a moment the superseded watch still holds the newest generation. A mark it
// began before the publish must still see the runtime that replaced it.
func TestApp_ConvergenceWatch_SupersededWatchCannotMarkThePublishedRuntime(t *testing.T) {
	app, _ := newConvergenceTestApp(t)
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	old, gen := app.CurrentRuntime(), app.convergenceGeneration()
	// The helper's cleanup stops whichever runtime is installed.
	t.Cleanup(func() { app.runtimeRef.Set(old) })

	// Holding the lock parks the mark at the lock that gates it; the runtime is
	// published while it waits, without its watch, as installPlan publishes one.
	app.convergenceMu.Lock()
	marked := make(chan bool, 1)
	go func() { marked <- app.markConvergenceDegraded(old, gen, "stale") }()
	wait.Until(t, 5*time.Second, "the mark waits for the convergence lock", func() bool {
		return parkedOnMutexIn("(*App).markConvergenceDegraded")
	})
	app.runtimeRef.Set(&goruntime.Runtime{})
	app.convergenceMu.Unlock()

	assert.False(t, wait.RequireReceive(t, marked, 5*time.Second),
		"the superseded watch cannot mark the runtime published in its place")
	degraded, _ := app.convergenceDegradedState()
	assert.False(t, degraded, "the published runtime is not degraded before its own watch judges it")
}

// parkedOnMutexIn reports whether a goroutine running fn is parked acquiring a
// sync.Mutex: the only signal that it has reached the lock without a hook.
func parkedOnMutexIn(fn string) bool {
	var stacks strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&stacks, 2)
	for g := range strings.SplitSeq(stacks.String(), "\n\n") {
		if strings.Contains(g, "[sync.Mutex.Lock") && strings.Contains(g, fn) {
			return true
		}
	}
	return false
}

// TestApp_ConvergenceWatch_CancelledParentSkipsWatch pins the shutdown-race guard: when the
// App-lifetime context (rootCtx) is already cancelled — as it is once Stop has run
// while a racing admin commit reaches installPlan — startConvergenceWatch must be
// a no-op (it must NOT call watchWg.Go, which would Add concurrently with Stop's
// watchWg.Wait and panic the shutdown goroutine).
func TestApp_ConvergenceWatch_CancelledParentSkipsWatch(t *testing.T) {
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

// TestApp_ConvergenceBudgetFloor proves the budget defaults to the floor
// when no session declares transport activation timing.
func TestApp_ConvergenceBudgetFloor(t *testing.T) {
	app := NewApp(testBootstrapCfg())
	require.Equal(t, bootstrapConvergenceBudgetFloor, app.convergenceBudget(&ports.BridgeConfig{}))
}

// refusedSession never connects, the way a session whose broker refuses its
// login stays: every health read reports it disconnected.
type refusedSession struct{ events chan ports.SessionEvent }

func (s *refusedSession) Start(context.Context) error                               { return nil }
func (s *refusedSession) Reconcile(context.Context, connectivity.SessionPlan) error { return nil }
func (s *refusedSession) Health(context.Context) ports.SessionHealth {
	return ports.SessionHealth{ServiceLevel: ports.ServiceLevelNone}
}
func (s *refusedSession) Events() <-chan ports.SessionEvent { return s.events }
func (s *refusedSession) Close(context.Context) error       { return nil }

// A runtime with no lease store whose only session is one a binding names, and
// which never connects, has not converged. The builder registers that session
// as exclusive with a deferred connect, but nothing can grant it a lease, so it
// is no standby waiting for one: readiness counts its real connection state and
// stays below subscribed, and the watch latches ConfigDegraded once the
// activation budget passes.
func TestApp_ConvergenceWatch_LeaselessBindingSessionThatNeverConnectsMarksDegraded(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("leaseless-refused"), goruntime.WithLogger(convDiscardLogger()))
	cfg := session.Config{SessionID: "dst-session", Exclusive: true, ConnectAfterLease: true}
	require.NoError(t, rt.RegisterSessionSender(cfg, &refusedSession{events: make(chan ports.SessionEvent, 1)}, discardSender{}))
	require.NoError(t, rt.Start(t.Context()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	app := NewApp(testBootstrapCfg(), WithLogger(convDiscardLogger()))
	app.clk = clk
	rec := &ports.RecordingExporter{}
	app.metricsExporter = rec
	app.runtimeRef.Set(rt)
	app.convergenceRt = rt
	app.appliedRef.Set(&ports.BridgeConfig{Version: 3})

	ctx, cancel := context.WithCancel(t.Context())
	watchDone := make(chan struct{})
	t.Cleanup(func() { cancel(); <-watchDone })
	go func() {
		defer close(watchDone)
		app.runConvergenceWatch(ctx, rt, app.convergenceGeneration(), 3*bootstrapConvergencePollInterval)
	}()
	// An armed poll timer means the watch took its deadline from the start
	// instant and its first poll did not count the runtime as converged.
	wait.Until(t, 2*time.Second, "the watch takes its first poll", func() bool {
		return clk.TimerCount() == 1 || isClosed(watchDone)
	})
	if isClosed(watchDone) {
		t.Fatalf("the watch counted readiness %s as converged for a session that never connected",
			rt.ReadinessLevel(context.Background()))
	}

	wait.Until(t, 5*time.Second, "the budget passes and the watch marks the version degraded", func() bool {
		if degraded, _ := app.degradedConfigWatch(); degraded {
			return true
		}
		clk.Advance(bootstrapConvergencePollInterval)
		return false
	})
	gauges := rec.FindEntries(shared.MetricConfigDegraded)
	require.NotEmpty(t, gauges)
	assert.InDelta(t, 1.0, gauges[len(gauges)-1].FValue, 0, "ConfigDegraded must read 1")
	assert.Less(t, rt.ReadinessLevel(context.Background()), bootstrapConvergenceReadyLevel)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

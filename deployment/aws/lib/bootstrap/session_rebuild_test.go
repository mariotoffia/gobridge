package bootstrap

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// errTransportGone is a session failure that only a fresh session clears.
var errTransportGone = fmt.Errorf("fake: %w", shared.ErrTransportClosedPermanently)

// rebuildWait bounds the wait for a session rebuild. The App does not hand its
// clock to the runtimes it builds, so the runtime paces the report with its
// rebuild backoff (at most 1s for a first failure) on the real clock.
const rebuildWait = 5 * time.Second

// failFirstSessionOf returns an onStart hook under which the first session
// built for id fails to start with errTransportGone, and every later one
// starts and is counted in started.
func failFirstSessionOf(id string, started *atomic.Int32) func(string, int) error {
	return func(startedID string, n int) error {
		if startedID != id {
			return nil
		}
		if n == 1 {
			return errTransportGone
		}
		started.Add(1)
		return nil
	}
}

// settleRebuild returns once a rebuild that holds the App's apply lock has
// finished.
func settleRebuild(app *App) {
	app.mu.Lock()
	defer app.mu.Unlock()
}

// Owner a's session fails in a way only a fresh session clears: the App
// rebuilds owner a's unit inside the installed runtime, while owner b keeps its
// session and the process keeps running.
func TestAppRebuildsAFailedSessionInsteadOfEndingTerminal(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	var copies atomic.Int32
	tf.onStart = failFirstSessionOf("a-s", &copies)
	var closedBeforeCopy atomic.Bool
	tf.onNewSession = func(id string) {
		if counts := tf.closeCounts(id); id == "a-s" && len(counts) == 1 && counts[0] == 1 {
			closedBeforeCopy.Store(true)
		}
	}
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt, applied, installed := app.CurrentRuntime(), app.CurrentAppliedConfig(), app.registryRef.Load()

	wait.Until(t, rebuildWait, "owner a's unit runs on a fresh session", func() bool { return copies.Load() > 0 })
	settleRebuild(app)

	assert.Same(t, rt, app.CurrentRuntime(), "the installed runtime is kept")
	assert.True(t, rt.IsRunning())
	assert.False(t, app.runtimeTerminal())
	assert.False(t, app.wedged.Load())
	assert.False(t, rt.SessionUnrecoverable("a-s"), "the fault leaves with the failed session")
	assert.True(t, closedBeforeCopy.Load(), "the failed session closes before its copy is built")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "owner a's failed session is closed once and replaced")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "owner b keeps its one session")
	assert.Same(t, applied, app.CurrentAppliedConfig(), "a rebuild is not a reload")
	assert.Same(t, installed, app.registryRef.Load(), "a rebuild is not a reload")
}

// A failed session that does not close may still hold its broker connection,
// so the App builds no copy beside it: it stops the runtime and wedges, which
// restarts the process (ADR-0004).
func TestAppSessionRebuildWedgesWhenTheUnitDoesNotStop(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	var copies atomic.Int32
	tf.onStart = failFirstSessionOf("a-s", &copies)
	tf.refuseClose("a-s", 1, errCloseRefused)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt := app.CurrentRuntime()

	wait.Until(t, rebuildWait, "the App wedges", app.wedged.Load)
	settleRebuild(app)

	assert.True(t, app.runtimeTerminal())
	assert.Nil(t, app.CurrentRuntime())
	assert.Nil(t, app.CurrentAppliedConfig())
	assert.False(t, rt.IsRunning(), "the runtime is stopped")
	assert.Len(t, tf.closeCounts("a-s"), 1, "no copy is built beside a session that did not close")
	assert.Zero(t, copies.Load())
}

// A rebuild that finds no unit holding the failed session in the installed
// configuration cannot serve it again in this runtime. The App does not keep
// the session silently unserved: it stops the runtime and wedges.
func TestAppSessionRebuildWedgesWhenNoUnitHoldsTheSession(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	var copies atomic.Int32
	tf.onStart = failFirstSessionOf("a-s", &copies)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	rec := &ports.RecordingExporter{}
	app.metricsExporter = rec
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt := app.CurrentRuntime()
	func() {
		app.mu.Lock() // hold the rebuild back once the report is taken
		defer app.mu.Unlock()
		wait.Until(t, rebuildWait, "the report of owner a's session is taken", func() bool {
			return len(rec.FindEntries(shared.MetricSessionRebuilds)) == 1
		})
		reg := *app.registryRef.Load()
		reg.cfg = inPlaceTestConfig("b")
		app.registryRef.Store(&reg)
	}()

	wait.Until(t, rebuildWait, "the App wedges", app.wedged.Load)
	settleRebuild(app)

	assert.True(t, app.runtimeTerminal())
	assert.Nil(t, app.CurrentRuntime())
	assert.False(t, rt.IsRunning(), "the runtime is stopped")
	assert.Len(t, tf.closeCounts("a-s"), 1, "no copy of the failed session is built")
	assert.Zero(t, copies.Load())
}

// An apply holds the apply lock and may run a session the registry it installs
// last does not hold yet. The handler takes a report made meanwhile, and the
// rebuild decides under the lock. Outside an apply, a report for a session the
// installed configuration lacks is refused.
func TestAppTakesASessionReportMadeDuringAnApply(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a")))
	rt := app.CurrentRuntime()

	assert.False(t, app.onSessionUnrecoverable("c-s", errTransportGone), "a session the configuration lacks is refused")
	func() {
		app.mu.Lock()
		defer app.mu.Unlock()
		assert.True(t, app.onSessionUnrecoverable("c-s", errTransportGone), "a report made during an apply is taken")
	}()
	settleRebuild(app)

	assert.Same(t, rt, app.CurrentRuntime())
	assert.True(t, rt.IsRunning())
	assert.False(t, app.runtimeTerminal())
	assert.False(t, app.wedged.Load())
}

// A report that arrives when the runtime no longer records the fault, because
// a reload or an earlier rebuild already replaced the session, rebuilds
// nothing.
func TestAppSessionRebuildIgnoresAStaleReport(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt, applied := app.CurrentRuntime(), app.CurrentAppliedConfig()

	app.rebuildSession("a-s")

	assert.Same(t, rt, app.CurrentRuntime())
	assert.True(t, rt.IsRunning())
	assert.False(t, app.runtimeTerminal())
	assert.False(t, app.wedged.Load())
	assert.Same(t, applied, app.CurrentAppliedConfig())
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "the healthy session is not retired")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"))
}

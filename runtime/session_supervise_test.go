package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// newSuperviseTestRuntime builds the smallest Runtime that superviseSession
// touches: a fake clock (deterministic backoff), a recording exporter (to
// observe the restart metric), and an initialised componentErrors map. healthy
// is seeded true on purpose so a test can prove the supervisor never flips it —
// the isolation invariant is that a quarantined/restarting session must
// NOT fail global readiness, which would get the whole pod restarted and defeat
// the isolation. logger is left nil (the supervisor guards nil); the zero mu
// and false terminal are ready to use.
func newSuperviseTestRuntime(clk *clocktest.Fake, rec *ports.RecordingExporter) *Runtime {
	return &Runtime{
		clk:             clk,
		metrics:         rec,
		componentErrors: make(map[string]error),
		healthy:         true,
		// Deterministic jitter: randFloat()==0 makes equalJitter return
		// backoff/2, so a test knows the exact wait it must Advance the fake
		// clock past to fire each backoff timer.
		randFloat: func() float64 { return 0 },
	}
}

// waitForBackoffTimer blocks until the supervisor has registered its backoff
// timer with the fake clock, so a following Advance cannot race ahead of the
// NewTimer call and leave the timer unfired (which would deadlock the retry).
// Polling TimerCount is the repo's standard fake-clock sync pattern; the poll
// interval paces the loop, it does not time the logic under test.
func waitForBackoffTimer(t *testing.T, clk *clocktest.Fake) {
	t.Helper()
	require.Eventually(t, func() bool { return clk.TimerCount() >= 1 },
		2*time.Second, time.Millisecond, "supervisor never registered a backoff timer")
}

// A session that keeps failing to reconnect/re-acquire its lease must be
// restarted in isolation: the failure is recorded + metered, but the runtime is
// neither cancelled nor marked unhealthy/terminal, so every unrelated route (and
// every other session) keeps running.
func TestSuperviseSession_RestartsTransientErrorWithoutTerminating(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	var calls atomic.Int32
	callCh := make(chan int, 8) // buffered so run never blocks signalling a call
	run := func(ctx context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		if n <= 2 {
			return errors.New("boom") // transient: reconnect / lease re-acquire
		}
		<-ctx.Done() // 3rd attempt stays up until the runtime shuts down
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseSession("s1", run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	// Two transient failures, each isolated and restarted after its backoff.
	for attempt := 1; attempt <= 2; attempt++ {
		select {
		case got := <-callCh:
			require.Equal(t, attempt, got)
		case <-time.After(2 * time.Second):
			t.Fatalf("run attempt %d not observed", attempt)
		}
		// Release the next retry by firing the backoff timer, but only once the
		// supervisor has actually registered it (avoid an Advance/NewTimer race).
		waitForBackoffTimer(t, clk)
		clk.Advance(30 * time.Second)
	}

	// Third attempt observed and now blocked until we shut down.
	select {
	case got := <-callCh:
		require.Equal(t, 3, got)
	case <-time.After(2 * time.Second):
		t.Fatal("third run attempt not observed")
	}

	// Exactly one restart metric per isolated failure (2), each tagged with the
	// failing session id, so the fault is observable without a global signal.
	restarts := rec.FindEntries(shared.MetricSessionRestarts)
	require.Len(t, restarts, 2)
	for _, e := range restarts {
		assert.Equal(t, int64(1), e.IValue)
		assert.Contains(t, e.Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "s1"})
	}

	// The transient faults were observable via the restart metric above; once
	// the 3rd attempt recovers and stays healthy the componentErrors entry is
	// cleared, so a recovered session leaves no phantom in failed_components ...
	assert.Nil(t, rt.ComponentErrors()["session:s1"],
		"a recovered session must not linger as a phantom failed component")
	// ... and the runtime is NOT torn down and NOT marked unhealthy: the
	// isolation invariant. A flipped healthy/terminal would fail readiness/
	// liveness and restart the whole pod — exactly what forbids.
	assert.False(t, rt.Terminal(), "session error must not make the runtime terminal")
	assert.True(t, rt.Healthy(), "session error must not flip the global healthy flag")

	// Shutdown: the in-flight (3rd) run unblocks on ctx and the supervisor
	// returns cleanly (nil), not as an error.
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}
}

// An ErrStaleFencingToken means another instance currently owns the
// lease. Previously the supervisor stopped cleanly, which permanently abandoned
// standby duty — the instance could never re-acquire when the active one later
// stepped down, silently removing the only failover target. The corrected
// contract treats a stale token as RESTARTABLE: the manager is re-run under
// jittered capped backoff so the instance keeps standby duty. It is metered and
// recorded like any isolated fault, and NEVER flips terminal/healthy.
func TestSuperviseSession_StaleFencingTokenRestartsKeepingStandbyDuty(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	var calls atomic.Int32
	callCh := make(chan int, 8)
	run := func(ctx context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		if n <= 2 {
			// Wrapped to prove errors.Is unwrapping (not identity) is what the
			// supervisor keys on.
			return fmt.Errorf("run failed: %w", shared.ErrStaleFencingToken)
		}
		<-ctx.Done() // 3rd attempt (won standby back / re-acquired) stays up
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseSession("s1", run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	// Two stale-token exits, each RESTARTED after its backoff — proving the
	// supervisor no longer abandons the session on a stale token.
	for attempt := 1; attempt <= 2; attempt++ {
		select {
		case got := <-callCh:
			require.Equal(t, attempt, got)
		case <-time.After(2 * time.Second):
			t.Fatalf("run attempt %d not observed", attempt)
		}
		waitForBackoffTimer(t, clk)
		clk.Advance(30 * time.Second)
	}

	select {
	case got := <-callCh:
		require.Equal(t, 3, got, "stale token must restart, keeping standby duty")
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor abandoned the session on a stale fencing token")
	}

	// Each isolated stale-token exit is metered as a restart.
	restarts := rec.FindEntries(shared.MetricSessionRestarts)
	require.Len(t, restarts, 2)
	// Isolation invariant preserved: never terminal, never unhealthy.
	assert.False(t, rt.Terminal())
	assert.True(t, rt.Healthy())

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}
}

// An ErrSessionUnrecoverable (a single-use session that
// cannot re-Start after a step-down Close) must be ESCALATED to terminal — the
// supervisor RETURNS the error (so startBackground flips terminal and the pod
// restarts with a fresh session) instead of looping on the dead instance, which
// would re-seize the lease via the store's same-owner fast path and wedge the
// cluster. It must NOT be metered as an ordinary restart.
func TestSuperviseSession_UnrecoverableSessionEscalatesToTerminal(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	var calls atomic.Int32
	run := func(_ context.Context) error {
		calls.Add(1)
		// Wrapped both ways, exactly like Manager.releaseAndReturn does.
		return fmt.Errorf("%w: %w", session.ErrSessionUnrecoverable, shared.ErrUnavailable)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseSession("s1", run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case err := <-done:
		// The supervisor RETURNS the error (does not swallow/retry) so
		// startBackground escalates to terminal.
		require.Error(t, err)
		assert.ErrorIs(t, err, session.ErrSessionUnrecoverable)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not escalate an unrecoverable session; it must not loop on the zombie")
	}

	assert.Equal(t, int32(1), calls.Load(), "an unrecoverable session must not be retried")
	assert.Empty(t, rec.FindEntries(shared.MetricSessionRestarts),
		"escalation is not an ordinary restart and must not meter one")
	assert.ErrorIs(t, rt.ComponentErrors()["session:s1"], session.ErrSessionUnrecoverable)
}

// Shutdown mid-run must be a clean stop: when ctx is cancelled while run is
// blocked, the supervisor returns nil and does not treat the resulting ctx
// error as a fault to meter or a reason to go terminal.
func TestSuperviseSession_CtxCancelDuringRunReturnsCleanly(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	started := make(chan struct{})
	var once sync.Once
	run := func(ctx context.Context) error {
		once.Do(func() { close(started) })
		<-ctx.Done() // block until shutdown
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	fn := rt.superviseSession("s1", run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("run never started")
	}
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}
	assert.Empty(t, rec.FindEntries(shared.MetricSessionRestarts))
	assert.False(t, rt.Terminal())
	assert.True(t, rt.Healthy())
}

// requireCall blocks until the supervised run signals its Nth invocation on
// callCh, failing the test if it does not arrive promptly.
func requireCall(t *testing.T, callCh <-chan int, want int) {
	t.Helper()
	select {
	case got := <-callCh:
		require.Equal(t, want, got)
	case <-time.After(2 * time.Second):
		t.Fatalf("run attempt %d not observed", want)
	}
}

// After a session recovers and runs healthy for a sustained window, a later
// unrelated blip must retry PROMPTLY (from minBackoff), not at the climbed cap:
// the supervisor resets the backoff ladder once a run outlives the stability
// window. Deterministic jitter (randFloat==0 => wait = backoff/2) lets the test
// prove the reset by the size of the advance that fires the next timer: only a
// reset (minBackoff/2 = 500ms) timer fires on a 500ms advance; an un-reset
// ladder would arm a >= 2s timer that 500ms cannot fire (the escalation hardening).
func TestSuperviseSession_BackoffResetsAfterSustainedRecovery(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	var calls atomic.Int32
	callCh := make(chan int, 8)
	release3 := make(chan struct{}) // unblocks the sustained 3rd run
	run := func(ctx context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		switch {
		case n <= 2:
			return errors.New("startup flap") // quick fails climb the ladder
		case n == 3:
			<-release3                     // stay healthy across the window ...
			return errors.New("late blip") // ... then fail once more
		default:
			<-ctx.Done() // 4th run stays up until shutdown
			return ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseSession("s1", run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	// Two quick transient failures climb the backoff ladder (1s -> 2s -> 4s).
	for attempt := 1; attempt <= 2; attempt++ {
		requireCall(t, callCh, attempt)
		waitForBackoffTimer(t, clk)
		clk.Advance(30 * time.Second) // fire whatever backoff is pending
	}

	// Third run stays healthy across the stability window, then fails.
	requireCall(t, callCh, 3)
	clk.Advance(30 * time.Second) // run 3 has now been up >= stabilityWindow
	close(release3)               // let run 3 return its late error

	// The reset makes the next wait minBackoff/2 = 500ms; a 500ms advance fires
	// it and the 4th run starts. Without the reset the timer would be >= 2s and
	// this advance would not fire it, so observing attempt 4 proves the reset.
	waitForBackoffTimer(t, clk)
	clk.Advance(500 * time.Millisecond)
	requireCall(t, callCh, 4)

	assert.False(t, rt.Terminal(), "isolated restart must not make the runtime terminal")
	assert.True(t, rt.Healthy(), "isolated restart must not flip the global healthy flag")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}
}

// A session that blips once and then recovers must NOT leave a permanent
// phantom in componentErrors / failed_components: the recorded fault is cleared
// before the (now healthy) retry, so /health stops reporting a stale failed
// component for the pod's remaining life (the escalation hardening).
func TestSuperviseSession_ComponentErrorClearedAfterRecovery(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	var calls atomic.Int32
	callCh := make(chan int, 8)
	run := func(ctx context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		if n == 1 {
			return errors.New("transient blip")
		}
		<-ctx.Done() // 2nd run recovers and stays healthy until shutdown
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseSession("s1", run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	// First run fails: the fault is recorded while the session is down.
	requireCall(t, callCh, 1)
	waitForBackoffTimer(t, clk)
	assert.NotNil(t, rt.ComponentErrors()["session:s1"],
		"a currently-failed session must be recorded for failed_components")

	// Fire the backoff; the retry recovers and stays healthy. The entry is
	// cleared before the retry runs, so by the time attempt 2 is observed the
	// phantom is gone.
	clk.Advance(30 * time.Second)
	requireCall(t, callCh, 2)
	assert.Nil(t, rt.ComponentErrors()["session:s1"],
		"a recovered session must not linger as a phantom failed component")
	assert.True(t, rt.Healthy())
	assert.False(t, rt.Terminal())

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}
}

// unrecoverableReport is one call of a session-unrecoverable handler.
type unrecoverableReport struct {
	sessionID string
	cause     error
}

// recordingUnrecoverableHandler returns a session-unrecoverable handler that
// records every call on the returned channel and answers take.
func recordingUnrecoverableHandler(take bool) (func(string, error) bool, chan unrecoverableReport) {
	reports := make(chan unrecoverableReport, 8)
	return func(sid string, cause error) bool {
		reports <- unrecoverableReport{sessionID: sid, cause: cause}
		return take
	}, reports
}

// A session whose transport is permanently closed, with nothing of the old
// session that needs a process restart, is cleared by a fresh session. With a
// handler installed the supervisor reports it there after the rebuild backoff
// and ends quietly instead of returning the error that makes the runtime
// terminal. The fault stays recorded, so the session reports not ready until a
// rebuild clears it.
func TestSuperviseSession_RebuildableUnrecoverableReportsToHandlerInsteadOfTerminal(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)
	handler, reports := recordingUnrecoverableHandler(true)
	WithSessionUnrecoverableHandler(handler)(rt)

	failure := fmt.Errorf("%w: %w", session.ErrSessionUnrecoverable, shared.ErrTransportClosedPermanently)
	var calls atomic.Int32
	run := func(context.Context) error {
		calls.Add(1)
		return failure
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rt.superviseSession("s1", run)(ctx) }()

	// The report waits out the first rebuild backoff: 1s, jittered to 500ms here.
	waitForBackoffTimer(t, clk)
	assert.Empty(t, reports, "the handler must not be told before the rebuild backoff ends")
	clk.Advance(time.Second)

	assert.NoError(t, wait.RequireReceive(t, done, 2*time.Second),
		"a session the handler takes must end its supervisor quietly, not make the runtime terminal")
	got := wait.RequireReceive(t, reports, 2*time.Second)
	assert.Equal(t, unrecoverableReport{sessionID: "s1", cause: failure}, got)
	assert.Empty(t, reports, "the handler is told exactly once")
	assert.Equal(t, int32(1), calls.Load(), "the supervisor must not run the dead session again")

	rebuilds := rec.FindEntries(shared.MetricSessionRebuilds)
	require.Len(t, rebuilds, 1)
	assert.Equal(t, int64(1), rebuilds[0].IValue)
	assert.Equal(t, []shared.Tag{{Key: shared.TagKeySessionID, Value: "s1"}}, rebuilds[0].Tags)
	assert.Empty(t, rec.FindEntries(shared.MetricSessionRestarts), "a reported session is not an ordinary restart")

	assert.True(t, rt.SessionUnrecoverable("s1"), "the fault stays recorded until a rebuild clears it")
	assert.False(t, rt.SessionUnrecoverable("s2"))
	assert.False(t, rt.Terminal())
	assert.True(t, rt.Healthy())
}

// A handler that refuses the rebuild leaves the escalation as it was: the
// supervisor returns the error, so the runtime goes terminal.
func TestSuperviseSession_HandlerRefusalKeepsTerminalEscalation(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)
	handler, reports := recordingUnrecoverableHandler(false)
	WithSessionUnrecoverableHandler(handler)(rt)

	failure := fmt.Errorf("%w: %w", session.ErrSessionUnrecoverable, shared.ErrTransportClosedPermanently)
	run := func(context.Context) error { return failure }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rt.superviseSession("s1", run)(ctx) }()

	waitForBackoffTimer(t, clk)
	clk.Advance(time.Second)

	err := wait.RequireReceive(t, done, 2*time.Second)
	assert.ErrorIs(t, err, session.ErrSessionUnrecoverable)
	assert.ErrorIs(t, err, shared.ErrTransportClosedPermanently)
	assert.Equal(t, unrecoverableReport{sessionID: "s1", cause: failure}, wait.RequireReceive(t, reports, 2*time.Second))
	assert.Empty(t, rec.FindEntries(shared.MetricSessionRebuilds), "a refused rebuild is not counted")
}

// requireTerminalWithoutReport runs a supervisor, with a handler that would take
// any rebuild, over a session that fails with failure, and requires that the
// supervisor returns the error at once: no backoff timer, no report, no rebuild.
func requireTerminalWithoutReport(t *testing.T, failure error) {
	t.Helper()
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)
	handler, reports := recordingUnrecoverableHandler(true)
	WithSessionUnrecoverableHandler(handler)(rt)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rt.superviseSession("s1", func(context.Context) error { return failure })(ctx) }()

	err := wait.RequireReceive(t, done, 2*time.Second)
	assert.ErrorIs(t, err, session.ErrSessionUnrecoverable)
	assert.Empty(t, reports, "the handler must not be told about a failure a rebuild cannot clear")
	assert.Zero(t, clk.TimerCount(), "the escalation must not wait out a rebuild backoff")
	assert.Empty(t, rec.FindEntries(shared.MetricSessionRebuilds))
	assert.True(t, rt.SessionUnrecoverable("s1"))
}

// A failure that leaves work of the old session behind needs a process restart:
// a rebuild beside that work would run two sessions on one identity. It stays
// terminal even though it also carries the permanent transport marker.
func TestSuperviseSession_ProcessRestartRequiredStaysTerminalWithHandler(t *testing.T) {
	requireTerminalWithoutReport(t, fmt.Errorf("%w: %w: %w",
		session.ErrSessionUnrecoverable, session.ErrProcessRestartRequired, shared.ErrTransportClosedPermanently))
}

// An unrecoverable failure without the permanent transport marker is not one a
// fresh session is known to clear, so it stays terminal.
func TestSuperviseSession_NonPermanentUnrecoverableStaysTerminalWithHandler(t *testing.T) {
	requireTerminalWithoutReport(t, fmt.Errorf("%w: %w", session.ErrSessionUnrecoverable, shared.ErrUnavailable))
}

// A shutdown while the supervisor waits out the rebuild backoff is a clean
// stop: the supervisor returns nil and the handler is never told.
func TestSuperviseSession_StopDuringRebuildBackoffIsCleanStop(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)
	handler, reports := recordingUnrecoverableHandler(true)
	WithSessionUnrecoverableHandler(handler)(rt)

	failure := fmt.Errorf("%w: %w", session.ErrSessionUnrecoverable, shared.ErrTransportClosedPermanently)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rt.superviseSession("s1", func(context.Context) error { return failure })(ctx) }()

	waitForBackoffTimer(t, clk)
	cancel()

	assert.NoError(t, wait.RequireReceive(t, done, 2*time.Second))
	assert.Empty(t, reports, "a stopped supervisor must not report the session")
	assert.Empty(t, rec.FindEntries(shared.MetricSessionRebuilds))
	assert.False(t, rt.Terminal())
}

// The wait before each report doubles per session up to the cap, independently
// for each session, and starts again at the minimum after a run that stayed up
// for the stability window.
func TestSuperviseSession_RebuildBackoffDoublesPerSessionAndResetsAfterStableRun(t *testing.T) {
	rt := newSuperviseTestRuntime(clocktest.NewAt(time.Unix(0, 0)), &ports.RecordingExporter{})
	const minBackoff, maxBackoff = time.Second, 30 * time.Second

	var got []time.Duration
	for range 7 {
		got = append(got, rt.nextRebuildBackoff("s1", false, minBackoff, maxBackoff))
	}
	assert.Equal(t, []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}, got)

	assert.Equal(t, time.Second, rt.nextRebuildBackoff("s2", false, minBackoff, maxBackoff),
		"each session keeps its own backoff")
	assert.Equal(t, time.Second, rt.nextRebuildBackoff("s1", true, minBackoff, maxBackoff),
		"a stable run starts the backoff again")
	assert.Equal(t, 2*time.Second, rt.nextRebuildBackoff("s1", false, minBackoff, maxBackoff))
}

// The rebuild backoff outlives the supervisor: the supervisor of a rebuilt
// session that fails again at once waits the doubled backoff before it reports
// it, so a session that keeps failing after each rebuild is not rebuilt in a
// tight loop.
func TestSuperviseSession_RebuildBackoffCarriesAcrossSupervisors(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)
	handler, reports := recordingUnrecoverableHandler(true)
	WithSessionUnrecoverableHandler(handler)(rt)

	failure := fmt.Errorf("%w: %w", session.ErrSessionUnrecoverable, shared.ErrTransportClosedPermanently)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	supervise := func() chan error {
		done := make(chan error, 1)
		go func() { done <- rt.superviseSession("s1", func(context.Context) error { return failure })(ctx) }()
		return done
	}

	// First supervisor: 1s backoff, jittered to 500ms.
	done := supervise()
	waitForBackoffTimer(t, clk)
	clk.Advance(500 * time.Millisecond)
	require.NoError(t, wait.RequireReceive(t, done, 2*time.Second))
	wait.RequireReceive(t, reports, 2*time.Second)

	// The rebuilt session's supervisor: 2s backoff, jittered to 1s, so 500ms
	// does not fire it.
	done = supervise()
	waitForBackoffTimer(t, clk)
	clk.Advance(500 * time.Millisecond)
	assert.Empty(t, reports, "the second report must wait the doubled backoff")
	assert.Equal(t, 1, clk.TimerCount(), "the doubled backoff timer must still be pending")
	clk.Advance(500 * time.Millisecond)
	require.NoError(t, wait.RequireReceive(t, done, 2*time.Second))
	wait.RequireReceive(t, reports, 2*time.Second)
	assert.Len(t, rec.FindEntries(shared.MetricSessionRebuilds), 2)
}

// A session that ran for the stability window and then stopped (its unit
// retired, or the runtime stopped) leaves no rebuild backoff behind; one that
// stopped sooner keeps it.
func TestSuperviseSession_StopAfterStableRunForgetsRebuildBackoff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		uptime  time.Duration
		forgets bool
	}{
		{name: "stable run", uptime: 30 * time.Second, forgets: true},
		{name: "short run", uptime: 29 * time.Second, forgets: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.NewAt(time.Unix(0, 0))
			rt := newSuperviseTestRuntime(clk, &ports.RecordingExporter{})
			rt.rebuildBackoff = map[string]time.Duration{"s1": 8 * time.Second}

			started := make(chan struct{})
			run := func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- rt.superviseSession("s1", run)(ctx) }()

			wait.RequireClosed(t, started, 2*time.Second)
			clk.Advance(tc.uptime)
			cancel()
			require.NoError(t, wait.RequireReceive(t, done, 2*time.Second))

			rt.mu.Lock()
			_, kept := rt.rebuildBackoff["s1"]
			rt.mu.Unlock()
			assert.Equal(t, !tc.forgets, kept)
		})
	}
}

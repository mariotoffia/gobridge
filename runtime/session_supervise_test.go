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
)

// newSuperviseTestRuntime builds the smallest Runtime that superviseSession
// touches: a fake clock (deterministic backoff), a recording exporter (to
// observe the restart metric), and an initialised componentErrors map. healthy
// is seeded true on purpose so a test can prove the supervisor never flips it —
// the C3-FU2 isolation invariant is that a quarantined/restarting session must
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
// every other session) keeps running (C3-FU2).
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
	// liveness and restart the whole pod — exactly what C3-FU2 forbids.
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

// Finding L11: a ErrStaleFencingToken means another instance currently owns the
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

// Finding C3-CRITICAL: a ErrSessionUnrecoverable (a single-use session that
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
// ladder would arm a >= 2s timer that 500ms cannot fire (C3-FU2 hardening).
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
// component for the pod's remaining life (C3-FU2 hardening).
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

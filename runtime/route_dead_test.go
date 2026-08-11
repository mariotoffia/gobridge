package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// routeHealthByID returns the RouteHealth projection for id, or the zero value
// when the route is absent from the DeepHealth snapshot.
func routeHealthByID(dh ports.DeepHealth, id string) ports.RouteHealth {
	for _, r := range dh.Routes {
		if r.ID == id {
			return r
		}
	}
	return ports.RouteHealth{}
}

// TestSuperviseRoute_RepeatedFlapsSurfaceRouteDead is the regression: a
// single-use receiver that fails instantly on every supervised restart settles
// at the 30s backoff cap and flaps forever behind a GREEN liveness probe. After
// routeDeadRestartThreshold consecutive sub-stability-window restarts the
// route must latch RouteDead=true in DeepHealth so ops can alert on the steady
// STATE — while the global healthy/terminal flags stay untouched (the per-route
// isolation invariant the keystone terminal/stopping seam must not regress).
func TestSuperviseRoute_RepeatedFlapsSurfaceRouteDead(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)
	// DeepHealth projects RouteDead off rt.entries keyed by config.ID; the route
	// must be registered and the runtime running for the snapshot to surface it.
	rt.running = true
	const routeID = "single-use-route"
	rt.entries = []*routeEntry{{config: RouteConfig{ID: routeID}}}

	var calls atomic.Int32
	callCh := make(chan int, 16) // buffered so run never blocks signalling a call
	run := func(_ context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		// Fails instantly every time: never reaches the stability window, so
		// every restart is a sub-window flap that advances the route_dead counter.
		return errors.New("single-use receiver: restart failed instantly")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseRoute(routeID, run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	// Run #1 fires immediately (no backoff before the first attempt); its flap is
	// recorded before the first backoff timer is armed.
	requireCall(t, callCh, 1)
	waitForBackoffTimer(t, clk)

	// Each subsequent restart is released by firing the pending backoff timer.
	// randFloat==0 makes equalJitter deterministic, and Advancing past the 30s
	// cap fires whatever wait is armed.
	for i := 2; i <= routeDeadRestartThreshold; i++ {
		clk.Advance(30 * time.Second)
		requireCall(t, callCh, i)
		waitForBackoffTimer(t, clk)

		// One restart short of the threshold the route must NOT yet be dead —
		// proving the signal is thresholded, not tripped by a single flap.
		if i == routeDeadRestartThreshold-1 {
			dh := rt.DeepHealth(context.Background())
			assert.False(t, routeHealthByID(dh, routeID).RouteDead,
				"route must not be flagged dead below the flap threshold")
		}
	}

	// routeDeadRestartThreshold consecutive flaps reached: route_dead latches.
	dh := rt.DeepHealth(context.Background())
	rh := routeHealthByID(dh, routeID)
	assert.True(t, rh.RouteDead,
		"route must latch route_dead after %d consecutive sub-stability-window restarts", routeDeadRestartThreshold)

	// The keystone isolation invariant must hold: a dead route is a per-route
	// STATE, never a global one. Liveness/readiness health and the
	// terminal/stopping seam must be untouched.
	assert.True(t, dh.Healthy, "route_dead must not flip the global healthy flag")
	assert.True(t, dh.Running, "route_dead must not stop the runtime")
	assert.True(t, rt.Healthy(), "route_dead must not flip Runtime.Healthy")
	assert.False(t, rt.Terminal(), "a dead single-use route must not make the runtime terminal")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}
}

// TestSuperviseRoute_RouteDeadClearsWhileStillRunning is the recovery case the
// exit-time reset alone cannot cover, and the one the ClearsAfterStableRecovery
// test masks by forcing the recovered run to return: a route flaps to the
// threshold, then its next run recovers and KEEPS RUNNING (never returns).
// superviseRoute resets the flap counter only when a run RETURNS after the
// stability window, so without the DeepHealth read-time liveness check this
// healthy long-running route would advertise route_dead forever. Asserts
// route_dead clears once the LIVE run outlives the stability window.
func TestSuperviseRoute_RouteDeadClearsWhileStillRunning(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)
	rt.running = true
	const routeID = "recovering-live-route"
	rt.entries = []*routeEntry{{config: RouteConfig{ID: routeID}}}

	var calls atomic.Int32
	callCh := make(chan int, 16)
	run := func(ctx context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		if n <= routeDeadRestartThreshold {
			return errors.New("startup flap") // drive the counter to the threshold
		}
		<-ctx.Done() // recovered: stay up until shutdown, never returning
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseRoute(routeID, run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	requireCall(t, callCh, 1)
	waitForBackoffTimer(t, clk)
	for i := 2; i <= routeDeadRestartThreshold; i++ {
		clk.Advance(30 * time.Second)
		requireCall(t, callCh, i)
		waitForBackoffTimer(t, clk)
	}
	// Threshold reached: dead (the last flapping run has returned, so no live run
	// suppresses the latch).
	assert.True(t, routeHealthByID(rt.DeepHealth(context.Background()), routeID).RouteDead)

	// Fire the last backoff → the recovering run starts and BLOCKS on ctx.Done: it
	// is now the live run and never returns, so the exit-time reset can never fire.
	clk.Advance(30 * time.Second)
	requireCall(t, callCh, routeDeadRestartThreshold+1)

	// While it is still running, advancing past the stability window must clear
	// route_dead via the read-time liveness check even though the flap counter is
	// still at the threshold (no run returned to reset it).
	clk.Advance(routeStabilityWindow + time.Second)
	assert.False(t, routeHealthByID(rt.DeepHealth(context.Background()), routeID).RouteDead,
		"route_dead must clear once the live run outlives the stability window, even though it never returns")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}
}

// live STATE, not a permanent latch: once a restart outlives the stability
// window the consecutive-flap counter resets, so a route that recovers stops
// reporting dead. Without the reset a route that flapped at startup and then
// healed would advertise a permanent false alarm.
func TestSuperviseRoute_RouteDeadClearsAfterStableRecovery(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)
	rt.running = true
	const routeID = "recovering-route"
	rt.entries = []*routeEntry{{config: RouteConfig{ID: routeID}}}

	var calls atomic.Int32
	callCh := make(chan int, 16)
	release := make(chan struct{}) // gates the sustained run's late return
	run := func(ctx context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		switch {
		case n <= routeDeadRestartThreshold:
			return errors.New("startup flap") // drive the counter to the threshold
		case n == routeDeadRestartThreshold+1:
			<-release                      // stay up across the stability window ...
			return errors.New("late blip") // ... then return: duration >= window resets the counter
		default:
			<-ctx.Done() // the final run stays up until shutdown
			return ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseRoute(routeID, run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	requireCall(t, callCh, 1)
	waitForBackoffTimer(t, clk)
	for i := 2; i <= routeDeadRestartThreshold; i++ {
		clk.Advance(30 * time.Second)
		requireCall(t, callCh, i)
		waitForBackoffTimer(t, clk)
	}
	// Threshold reached: dead.
	assert.True(t, routeHealthByID(rt.DeepHealth(context.Background()), routeID).RouteDead)

	// Fire the last backoff → the recovering run starts. It stays up while we
	// advance past the stability window, so when it finally returns the supervisor
	// takes the reset branch (Since(runStart) >= stabilityWindow).
	clk.Advance(30 * time.Second)
	requireCall(t, callCh, routeDeadRestartThreshold+1)
	clk.Advance(30 * time.Second) // the run now outlives the stability window
	close(release)                // let it return its late error → triggers the flap reset

	// After the reset the counter is cleared, so route_dead clears. Wait for the
	// supervisor to arm the post-reset backoff timer (which happens right after
	// the reset) so the assertion cannot race the reset.
	waitForBackoffTimer(t, clk)
	assert.False(t, routeHealthByID(rt.DeepHealth(context.Background()), routeID).RouteDead,
		"route_dead must clear once the route recovers past the stability window")

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

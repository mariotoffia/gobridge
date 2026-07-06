package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Finding L12 (per-route supervision): a permanently-failing route must be
// isolated with jittered capped backoff and restarted in place. It must NEVER
// flip the global terminal/healthy flags (which would CrashLoopBackOff the whole
// pod and kill every healthy co-tenant route), and it must stay observable via
// MetricRouteRestarts + componentErrors so a genuinely-permanent fault is
// visible without a global signal.
func TestSuperviseRoute_PermanentFailureIsolatedNotTerminal(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	var calls atomic.Int32
	callCh := make(chan int, 8)
	permanent := errors.New("queue deleted; access denied")
	run := func(_ context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		return permanent // never recovers
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseRoute("r1", run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	// Observe three isolated restarts, each released by firing the backoff timer.
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case got := <-callCh:
			require.Equal(t, attempt, got)
		case <-time.After(2 * time.Second):
			t.Fatalf("route run attempt %d not observed", attempt)
		}
		// The failing route is recorded as a component fault BEFORE its backoff.
		require.Eventually(t, func() bool {
			return rt.ComponentErrors()["route:r1"] != nil
		}, 2*time.Second, time.Millisecond, "route fault not recorded")
		waitForBackoffTimer(t, clk)
		clk.Advance(30 * time.Second)
	}

	// Each isolated failure emitted exactly one restart metric tagged by route.
	restarts := rec.FindEntries(shared.MetricRouteRestarts)
	require.GreaterOrEqual(t, len(restarts), 3)
	for _, e := range restarts {
		assert.Equal(t, int64(1), e.IValue)
		assert.Contains(t, e.Tags, shared.Tag{Key: shared.TagKeyRouteID, Value: "r1"})
	}

	// The whole point: a permanently-failing route never trips the global flags,
	// so /live and /health stay green and healthy co-tenant routes keep running.
	assert.False(t, rt.Terminal(), "a failing route must not make the runtime terminal")
	assert.True(t, rt.Healthy(), "a failing route must not flip the global healthy flag")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("route supervisor did not return after ctx cancel")
	}
}

// A route that returns nil while the runtime is still live (its receiver loop
// ended without a shutdown signal) is an anomalous stop: superviseRoute must
// surface it as errRouteUnexpectedStop and restart, not silently strand a dead
// route while the runtime keeps advertising it ready.
func TestSuperviseRoute_UnexpectedNilStopRestarts(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	var calls atomic.Int32
	callCh := make(chan int, 8)
	run := func(ctx context.Context) error {
		n := int(calls.Add(1))
		callCh <- n
		if n == 1 {
			return nil // anomalous clean stop while runtime is live
		}
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseRoute("r1", run)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case got := <-callCh:
		require.Equal(t, 1, got)
	case <-time.After(2 * time.Second):
		t.Fatal("first run not observed")
	}
	waitForBackoffTimer(t, clk)
	clk.Advance(30 * time.Second)

	select {
	case got := <-callCh:
		require.Equal(t, 2, got, "nil stop must trigger a restart")
	case <-time.After(2 * time.Second):
		t.Fatal("route did not restart after unexpected nil stop")
	}
	assert.False(t, rt.Terminal())
	assert.True(t, rt.Healthy())

	cancel()
	<-done
}

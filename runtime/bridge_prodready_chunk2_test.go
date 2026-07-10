package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// TestSuperviseRoute_TerminalReceiverEscalates proves HIGH-2/HIGH-3/HIGH-4
// supervision: a route runner that declares itself UNRESTARTABLE (Run returns an
// error wrapping route.ErrRouteTerminal — a closed single-use receiver, or a
// wedge after a hung sender / abandoned-processor storm) is NOT restarted in
// place. superviseRoute returns the terminal error immediately — which flips the
// runtime terminal in startBackground so an orchestrator restarts the pod with
// freshly-built transports — instead of flapping the same dead instance at the
// backoff cap forever behind green liveness.
//
// Mutation check: delete the `if errors.Is(err, route.ErrRouteTerminal)` branch
// in superviseRoute and this fails — the supervisor loops (run is called again
// after a backoff) instead of escalating on the first terminal return.
func TestSuperviseRoute_TerminalReceiverEscalates(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	rt := newSuperviseTestRuntime(clk, rec)

	var calls atomic.Int32
	run := func(_ context.Context) error {
		calls.Add(1)
		return route.ErrRouteReceiverClosed // wraps route.ErrRouteTerminal
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := rt.superviseRoute("r1", run)

	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, route.ErrRouteTerminal,
			"a terminal route condition must be ESCALATED (returned), not restarted in place")
	case <-time.After(2 * time.Second):
		t.Fatal("superviseRoute did not escalate a terminal route error; it is flapping the dead instance")
	}

	// The runner was entered exactly once: no backoff-then-restart loop.
	assert.Equal(t, int32(1), calls.Load(),
		"a terminal route must NOT be re-run; escalate to the orchestrator instead")

	// The escalation stays observable: the route fault is recorded and the
	// restart metric fired exactly once before returning.
	assert.NotNil(t, rt.ComponentErrors()["route:r1"], "terminal route fault must be recorded")
	restarts := rec.FindEntries(shared.MetricRouteRestarts)
	require.Len(t, restarts, 1)
	assert.Contains(t, restarts[0].Tags, shared.Tag{Key: shared.TagKeyRouteID, Value: "r1"})
}

// TestEffectiveStoreCloseGrace_DerivesFromPolicyTimeouts proves the inherited R1
// handoff: the manager-close grace Stop waits for must be COHERENT with the
// drainer's worst-case single in-flight send (SendTimeout + min(SendTimeout,5s))
// so a legitimate final drainer send is never cut off mid-flight — which would
// clear the lease and make the drainer's post-send fence refuse the final
// Complete, resurfacing the record on restart as an avoidable duplicate.
//
// Mutation check: replace rt.effectiveStoreCloseGrace() with the bare
// storeCloseGrace const at the Stop grace-wait and the "raises above the floor"
// sub-case fails (35s/45s collapse back to 15s).
func TestEffectiveStoreCloseGrace_DerivesFromPolicyTimeouts(t *testing.T) {
	policyEntry := func(sendTimeout time.Duration) *routeEntry {
		return &routeEntry{config: RouteConfig{
			Policy: routing.RoutePolicy{SendTimeout: sendTimeout},
		}}
	}

	t.Run("no entries: the floor applies", func(t *testing.T) {
		rt := &Runtime{}
		assert.Equal(t, storeCloseGrace, rt.effectiveStoreCloseGrace())
	})

	t.Run("small SendTimeout: floor still dominates", func(t *testing.T) {
		rt := &Runtime{entries: []*routeEntry{policyEntry(2 * time.Second)}}
		// worst = 2s + min(2s,5s) = 4s < 15s floor.
		assert.Equal(t, storeCloseGrace, rt.effectiveStoreCloseGrace())
	})

	t.Run("default SendTimeout raises the grace above the floor", func(t *testing.T) {
		// WithDefaults() gives SendTimeout=30s -> worst = 30s + min(30s,5s) = 35s.
		rt := &Runtime{entries: []*routeEntry{policyEntry(0)}}
		assert.Equal(t, 35*time.Second, rt.effectiveStoreCloseGrace())
	})

	t.Run("takes the MAX worst-case across entries", func(t *testing.T) {
		rt := &Runtime{entries: []*routeEntry{
			policyEntry(20 * time.Second), // worst 25s
			policyEntry(40 * time.Second), // worst 45s
			policyEntry(10 * time.Second), // worst 15s
		}}
		// grace = max(15s floor, 25s, 45s, 15s) = 45s.
		assert.Equal(t, 45*time.Second, rt.effectiveStoreCloseGrace())
	})
}

// TestEffectiveStoreCloseGrace_NeverBelowFloor is a defensive property: whatever
// the configured policies, the derived grace is never below the storeCloseGrace
// floor so a genuinely stuck drainer's lease still eventually releases (the
// "lesser-evil" single-owner-failover guarantee stays true).
func TestEffectiveStoreCloseGrace_NeverBelowFloor(t *testing.T) {
	rt := &Runtime{entries: []*routeEntry{
		{config: RouteConfig{Policy: routing.RoutePolicy{SendTimeout: -1}}},
		{config: RouteConfig{Policy: routing.RoutePolicy{SendTimeout: time.Millisecond}}},
	}}
	if got := rt.effectiveStoreCloseGrace(); got < storeCloseGrace {
		t.Fatalf("effectiveStoreCloseGrace = %v, must never drop below the %v floor", got, storeCloseGrace)
	}
}

// TestB6_ClampedStoreCloseGrace_BoundedByShutdownDeadline is the regression test
// for B6. The manager-close grace-wait detaches from the caller ctx via
// context.WithoutCancel, so an UNCLAMPED policy-derived grace (up to
// SendTimeout + min(SendTimeout,5s) — ~65s for a 60s SendTimeout) can outlive the
// platform's own kill budget (ECS StopTimeout / K8s terminationGracePeriod,
// default 60s) → SIGKILL mid-drain → the avoidable duplicate + lost in-flight the
// coherence raise was meant to prevent. clampedStoreCloseGrace bounds the wait to
// the incoming shutdown ctx's remaining deadline (minus a small margin).
//
// Mutation: drop the clamp (return rt.effectiveStoreCloseGrace() directly at the
// grace-wait) and the derived 65s wait exceeds the 20s deadline → the ≤20s
// assertion fails.
func TestB6_ClampedStoreCloseGrace_BoundedByShutdownDeadline(t *testing.T) {
	// SendTimeout=60s → effectiveStoreCloseGrace = 60s + min(60s,5s) = 65s.
	// clk is clock.System (real wall-clock) so deadline.Sub(rt.clk.Now()) lines up
	// with the real-time ctx deadlines below, exactly as production (New defaults
	// rt.clk to clock.System).
	rt := &Runtime{
		clk: clock.System,
		entries: []*routeEntry{
			{config: RouteConfig{Policy: routing.RoutePolicy{SendTimeout: 60 * time.Second}}},
		},
	}
	require.Equal(t, 65*time.Second, rt.effectiveStoreCloseGrace(),
		"precondition: the policy must derive a grace that exceeds the shutdown deadline")

	t.Run("deadline shorter than derived grace clamps to the deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(20*time.Second))
		defer cancel()

		got := rt.clampedStoreCloseGrace(ctx)
		// Must never outlive the platform's 20s budget...
		assert.LessOrEqual(t, got, 20*time.Second,
			"B6: a detached grace-wait longer than the shutdown deadline would be SIGKILLed mid-drain")
		// ...and must actually be clamped BELOW the derived 65s (not the raw grace).
		assert.Less(t, got, 65*time.Second, "B6: the derived grace must be clamped, not passed through")
		// A little headroom for the close phase that follows the wait.
		assert.LessOrEqual(t, got, 20*time.Second-storeCloseGraceMargin+time.Millisecond)
		assert.Positive(t, got, "still a positive wait: there is time left before the deadline")
	})

	t.Run("no deadline: the derived grace stands", func(t *testing.T) {
		// A deadline-less caller cannot be SIGKILLed by a platform budget this
		// layer can observe, so the full policy-derived grace is preserved.
		assert.Equal(t, 65*time.Second, rt.clampedStoreCloseGrace(context.Background()))
	})

	t.Run("already-expired deadline clamps to zero, never negative", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		got := rt.clampedStoreCloseGrace(ctx)
		assert.GreaterOrEqual(t, got, time.Duration(0), "clamp must floor at zero, never pass a negative to WithTimeout")
		assert.LessOrEqual(t, got, time.Duration(0)) // exactly zero: no time left to wait
	})
}

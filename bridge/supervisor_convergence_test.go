package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// the convergence budget derives from the largest transport-declared
// post-takeover activation, floored so transports without the capability
// still get one connect + one reconcile of patience.
func TestConvergenceBudget_DerivesFromTransportTiming(t *testing.T) {
	t.Run("floor when no session declares timing", func(t *testing.T) {
		cfg := &ports.BridgeConfig{Sessions: []ports.SessionDef{
			{ID: "plain", Config: failoverNoTimingPluginConfig{}},
		}}
		assert.Equal(t, convergenceBudgetFloor, convergenceBudget(cfg))
	})

	t.Run("largest declared activation wins", func(t *testing.T) {
		cfg := &ports.BridgeConfig{Sessions: []ports.SessionDef{
			{ID: "slow", SessionMode: string(connectivity.SessionExclusive),
				Config: failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 4 * time.Minute}}},
			{ID: "fast", SessionMode: string(connectivity.SessionExclusive),
				Config: failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 90 * time.Second}}},
		}}
		assert.Equal(t, 4*time.Minute, convergenceBudget(cfg))
	})

	t.Run("declared activation below the floor keeps the floor", func(t *testing.T) {
		cfg := &ports.BridgeConfig{Sessions: []ports.SessionDef{
			{ID: "quick", SessionMode: string(connectivity.SessionExclusive),
				Config: failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: time.Second}}},
		}}
		assert.Equal(t, convergenceBudgetFloor, convergenceBudget(cfg))
	})
}

// an applied runtime that never reaches broker state within the
// activation budget flips the supervisor into the DISTINCT
// applied-but-not-converged degraded state — a reload that reported success
// while the transport is down must not stay green. When the sessions later
// converge (per-session supervision retries forever), the watcher clears the
// state it set.
func TestSupervisor_ConvergenceWatch_MarksAppliedNotConvergedThenClearsOnConvergence(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s := NewSupervisor(WithSupervisorClock(clk))

	// An unstarted runtime reports LevelLive (< LevelSubscribed): the exact
	// shape of a committed reload whose transport never reached the broker.
	rt := runtime.New(runtime.WithInstanceID("convergence-watch"))
	s.mu.Lock()
	s.rt = rt
	s.cfg = &ports.BridgeConfig{Version: 7}
	s.mu.Unlock()

	watchDone := make(chan struct{})
	const budget = 4 * time.Second
	go func() {
		defer close(watchDone)
		s.runConvergenceWatch(t.Context(), rt, budget)
	}()

	// Drive the fake clock past the budget; each advance fires one poll tick.
	require.Eventually(t, func() bool {
		clk.Advance(convergencePollInterval)
		degraded, reason := s.Degraded()
		return degraded && strings.Contains(reason, "not converged")
	}, 5*time.Second, 5*time.Millisecond,
		"budget expiry must surface the applied-but-not-converged degraded state")

	degraded, reason := s.Degraded()
	require.True(t, degraded)
	assert.Contains(t, reason, "config version 7")

	// The sessions "converge": starting the runtime makes it Running+Healthy,
	// and an instance carrying no sessions has nothing left to connect — the
	// convergence rule for an empty runtime.
	require.NoError(t, rt.Start(t.Context()))
	t.Cleanup(func() { _ = rt.Stop(t.Context()) })

	require.Eventually(t, func() bool {
		clk.Advance(convergencePollInterval)
		degraded, _ := s.Degraded()
		return !degraded
	}, 5*time.Second, 5*time.Millisecond,
		"late convergence must clear the watcher-owned degraded state")

	select {
	case <-watchDone:
	case <-t.Context().Done():
		t.Fatal("convergence watcher did not terminate after convergence")
	}
}

// Review finding (pause interaction): a deliberate StopBridge must clear a
// convergence-owned degraded mark — a paused bridge is not "applied but not
// converged", and the stale mark would otherwise scream "revert the config"
// until an unrelated future reload. A foreign degraded cause must survive
// the pause untouched.
func TestSupervisor_StopBridgeClearsConvergenceOwnedDegradedOnly(t *testing.T) {
	t.Run("convergence-owned mark cleared on pause", func(t *testing.T) {
		s := NewSupervisor()
		rt := runtime.New(runtime.WithInstanceID("pause-clear"))
		s.mu.Lock()
		s.rt = rt
		s.mu.Unlock()
		_, marked := s.markConvergenceDegraded(rt, ports.LevelLive, time.Minute)
		require.True(t, marked)

		require.NoError(t, s.StopBridge(t.Context()))

		degraded, reason := s.Degraded()
		assert.False(t, degraded, "a deliberate pause invalidates the convergence observation")
		assert.Empty(t, reason)
		s.mu.RLock()
		paused := s.paused
		s.mu.RUnlock()
		assert.True(t, paused)
	})

	t.Run("foreign degraded cause survives pause", func(t *testing.T) {
		s := NewSupervisor()
		rt := runtime.New(runtime.WithInstanceID("pause-foreign"))
		s.mu.Lock()
		s.rt = rt
		s.mu.Unlock()
		s.markDegraded("config change stream closed; live reconfiguration unavailable")

		require.NoError(t, s.StopBridge(t.Context()))

		degraded, reason := s.Degraded()
		assert.True(t, degraded, "StopBridge must never clear a degraded cause it does not own")
		assert.Contains(t, reason, "config change stream closed")
	})
}

// Review finding (pause interaction): a watcher must treat a paused bridge as
// not-current — a stopped runtime reports LevelLive forever, so continuing to
// observe it would convert an admin pause into a false alarm — and marking
// through a paused supervisor must be refused.
func TestSupervisor_ConvergenceWatch_AbandonsWhenPaused(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s := NewSupervisor(WithSupervisorClock(clk))
	rt := runtime.New(runtime.WithInstanceID("pause-abandon"))
	s.mu.Lock()
	s.rt = rt
	s.paused = true
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runConvergenceWatch(t.Context(), rt, time.Second)
	}()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("watcher did not abandon a paused bridge")
	}
	degraded, _ := s.Degraded()
	assert.False(t, degraded)
	_, marked := s.markConvergenceDegraded(rt, ports.LevelLive, time.Second)
	assert.False(t, marked, "marking through a paused supervisor must be refused")
}

// Review finding (pause interaction): a successor watcher observing
// convergence clears a convergence-owned mark left by a PREDECESSOR watcher
// (e.g. marked before a pause/resume) — the alarm is factually resolved even
// though this watcher instance never marked it.
func TestSupervisor_ConvergenceWatch_ClearsPredecessorMarkOnConvergence(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s := NewSupervisor(WithSupervisorClock(clk))
	rt := runtime.New(runtime.WithInstanceID("predecessor-clear"))
	require.NoError(t, rt.Start(t.Context())) // empty runtime: LevelFull immediately
	t.Cleanup(func() { _ = rt.Stop(t.Context()) })
	s.mu.Lock()
	s.rt = rt
	s.degraded = true
	s.degradedReason = "config version 3 applied but transport sessions have not converged"
	s.degradedByConvergence = true
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runConvergenceWatch(t.Context(), rt, time.Minute)
	}()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("watcher did not terminate on convergence")
	}
	degraded, _ := s.Degraded()
	assert.False(t, degraded, "convergence resolves a predecessor watcher's convergence-owned mark")
}

// a reload that says the same thing as the running one keeps the runtime and
// only adopts the new document: the supervisor reports the new version while
// the watch started by the original swap is still running. The watch's
// diagnostics must name the document the supervisor holds when the diagnostic
// is written, so an operator told "config version N" looks at the version the
// bridge reports as running. Adoption must not move the watch's deadline
// either — the budget belongs to the runtime, and the runtime did not change.
func TestConvergenceWatch_DiagnosticsNameTheAdoptedDocument(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s := NewSupervisor(WithSupervisorClock(clk))

	// An unstarted runtime reports LevelLive and never converges on its own.
	rt := runtime.New(runtime.WithInstanceID("adopted-document"))
	s.mu.Lock()
	s.rt = rt
	s.cfg = &ports.BridgeConfig{Version: 1}
	s.mu.Unlock()

	const ticksToBudget = 10
	const budget = ticksToBudget * convergencePollInterval
	start := clk.Now()

	ctx, cancel := context.WithCancel(t.Context())
	watchDone := make(chan struct{})
	t.Cleanup(func() { cancel(); <-watchDone })
	go func() {
		defer close(watchDone)
		s.runConvergenceWatch(ctx, rt, budget)
	}()
	// The watch reads the clock to compute its deadline and then arms its poll
	// timer, so an armed timer proves the deadline was taken from the start
	// instant rather than from an already-advanced clock.
	require.Eventually(t, func() bool { return clk.TimerCount() == 1 }, time.Second, time.Millisecond,
		"the convergence watch must arm its poll timer")

	advance := func(ticks int) {
		for range ticks {
			clk.Advance(convergencePollInterval)
		}
	}

	// Half a budget in, an equivalent document is adopted the way the
	// no-op reload path adopts one: the applied config moves, the runtime stays.
	advance(ticksToBudget / 2)
	s.mu.Lock()
	s.cfg = &ports.BridgeConfig{Version: 2}
	s.mu.Unlock()

	// Advance to exactly the ORIGINAL expiry and no further. A deadline pushed
	// out by the adoption would leave the watch silent here.
	advance(ticksToBudget / 2)
	require.Eventually(t, func() bool {
		degraded, _ := s.Degraded()
		return degraded
	}, 2*time.Second, time.Millisecond,
		"the original budget must still expire on schedule after an equivalent document is adopted")

	degraded, reason := s.Degraded()
	require.True(t, degraded)
	assert.Contains(t, reason, "config version 2",
		"the degraded reason must name the document the supervisor holds now")
	assert.NotContains(t, reason, "config version 1",
		"naming the superseded document sends the operator to the wrong version")
	assert.Equal(t, budget, clk.Now().Sub(start),
		"the mark belongs at the original budget expiry, not a budget later")
}

// a watcher whose runtime was replaced by a later swap must abandon
// silently — the successor swap owns the convergence signal — and must never
// clobber a degraded state it does not own.
func TestSupervisor_ConvergenceWatch_AbandonsWhenRuntimeReplaced(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s := NewSupervisor(WithSupervisorClock(clk))

	oldRt := runtime.New(runtime.WithInstanceID("convergence-old"))
	newRt := runtime.New(runtime.WithInstanceID("convergence-new"))
	s.mu.Lock()
	s.rt = newRt // the old runtime is already superseded
	s.degraded = true
	s.degradedReason = "someone else's degraded cause"
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runConvergenceWatch(t.Context(), oldRt, time.Second)
	}()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("superseded watcher did not abandon")
	}

	degraded, reason := s.Degraded()
	assert.True(t, degraded, "a superseded watcher must not clear foreign degraded state")
	assert.Equal(t, "someone else's degraded cause", reason)
	_, marked := s.markConvergenceDegraded(oldRt, ports.LevelLive, time.Second)
	assert.False(t, marked, "marking through a superseded runtime must be refused")
}

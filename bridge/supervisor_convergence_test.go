package bridge

import (
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

// MQTT-R1: the convergence budget derives from the largest transport-declared
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

// MQTT-R1: an applied runtime that never reaches broker state within the
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
	s.mu.Unlock()

	watchDone := make(chan struct{})
	const budget = 4 * time.Second
	go func() {
		defer close(watchDone)
		s.runConvergenceWatch(t.Context(), rt, 7, budget)
	}()

	// Drive the fake clock past the budget; each advance fires one poll tick.
	require.Eventually(t, func() bool {
		clk.Advance(convergencePollInterval)
		degraded, reason := s.Degraded()
		return degraded && strings.Contains(reason, "not converged")
	}, 5*time.Second, 5*time.Millisecond,
		"budget expiry must surface the applied-but-not-converged degraded state (MQTT-R1)")

	degraded, reason := s.Degraded()
	require.True(t, degraded)
	assert.Contains(t, reason, "config version 7")

	// The sessions "converge": an empty started runtime is Running+Healthy
	// with no sessions, which classifies at LevelFull ≥ LevelSubscribed.
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

// MQTT-R1: a watcher whose runtime was replaced by a later swap must abandon
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
		s.runConvergenceWatch(t.Context(), oldRt, 3, time.Second)
	}()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("superseded watcher did not abandon")
	}

	degraded, reason := s.Degraded()
	assert.True(t, degraded, "a superseded watcher must not clear foreign degraded state")
	assert.Equal(t, "someone else's degraded cause", reason)
	assert.False(t, s.markConvergenceDegraded(oldRt, "stale mark"),
		"marking through a superseded runtime must be refused")
}

package paho

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// ═══════════════════════════════════════════════════════════════════════════
// (MEDIUM): no reconnect jitter → fleet thundering herd.
//
// The old dial wired autopaho.NewConstantBackoff(reconnect_delay): every
// instance that lost the same broker retried on identical wall-clock
// boundaries, hammering the broker in synchronised waves as it returned.
//
// Fix: a JITTERED EXPONENTIAL backoff (reconnectBackoff) — the base delay
// grows from reconnect_delay up to reconnect_max_delay, then equal-jitter
// spreads each attempt over [d/2, d). newReconnectBackoff composes it with the
// escalating session-takeover penalty.
// ═══════════════════════════════════════════════════════════════════════════

// expectedBaseDelay mirrors reconnectBackoff's capped exponential (pre-jitter)
// so tests can assert the [d/2, d) envelope.
func expectedBaseDelay(attempt int, base, maxDelay time.Duration, factor float64) time.Duration {
	d := float64(base)
	for i := 1; i < attempt; i++ {
		d *= factor
		if d >= float64(maxDelay) {
			return maxDelay
		}
	}
	if d > float64(maxDelay) {
		return maxDelay
	}
	return time.Duration(d)
}

// TestReconnectBackoff_ZeroBeforeFirstAttempt asserts attempt 0 (autopaho's
// pre-first-connect call) is always 0 delay.
func TestReconnectBackoff_ZeroBeforeFirstAttempt(t *testing.T) {
	require.Equal(t, time.Duration(0),
		reconnectBackoff(0, 10*time.Second, 2*time.Minute, 2.0, func() float64 { return 0.5 }))
}

// TestReconnectBackoff_WithinEnvelope asserts every attempt's jittered delay
// lands in [d/2, d) for the capped-exponential base d, across the full jitter
// range (randFloat 0 → ~1).
func TestReconnectBackoff_WithinEnvelope(t *testing.T) {
	const (
		base   = 10 * time.Second
		maxDel = 2 * time.Minute
		factor = 2.0
	)
	randValues := []float64{0, 0.25, 0.5, 0.75, 0.999999}
	for attempt := 1; attempt <= 8; attempt++ {
		d := expectedBaseDelay(attempt, base, maxDel, factor)
		for _, rf := range randValues {
			got := reconnectBackoff(attempt, base, maxDel, factor, func() float64 { return rf })
			require.GreaterOrEqual(t, got, d/2,
				"attempt %d rand %v: delay must be >= d/2", attempt, rf)
			require.Less(t, got, d,
				"attempt %d rand %v: delay must be < d (equal-jitter upper bound)", attempt, rf)
		}
	}
}

// TestReconnectBackoff_NonConstant_GrowsThenCaps asserts the delays are NOT
// constant (the regression): with a fixed jitter fraction they grow
// exponentially per attempt until the ceiling, then stay capped. A constant
// backoff (the old NewConstantBackoff) would fail the strict-growth assertion.
func TestReconnectBackoff_NonConstant_GrowsThenCaps(t *testing.T) {
	const (
		base   = 10 * time.Second
		maxDel = 2 * time.Minute
		factor = 2.0
	)
	half := func() float64 { return 0.5 }

	d1 := reconnectBackoff(1, base, maxDel, factor, half) // 7.5s
	d2 := reconnectBackoff(2, base, maxDel, factor, half) // 15s
	d3 := reconnectBackoff(3, base, maxDel, factor, half) // 30s
	d4 := reconnectBackoff(4, base, maxDel, factor, half) // 60s
	d5 := reconnectBackoff(5, base, maxDel, factor, half) // capped: min(160,120)=120 → 90s
	d6 := reconnectBackoff(6, base, maxDel, factor, half) // capped → 90s

	require.True(t, d1 < d2 && d2 < d3 && d3 < d4,
		"delays must grow exponentially before the cap (not constant): %v<%v<%v<%v", d1, d2, d3, d4)
	require.Equal(t, d5, d6, "once the max-delay ceiling is hit the (jittered) delay is stable")
	require.LessOrEqual(t, d5, maxDel, "the capped delay never exceeds reconnect_max_delay")
}

// TestReconnectBackoff_JitterVariesWithRand asserts the jitter actually spreads
// the delay: the SAME attempt yields different delays for different rand draws
// (the anti-thundering-herd property).
func TestReconnectBackoff_JitterVariesWithRand(t *testing.T) {
	const (
		base   = 10 * time.Second
		maxDel = 2 * time.Minute
		factor = 2.0
	)
	low := reconnectBackoff(3, base, maxDel, factor, func() float64 { return 0 })
	high := reconnectBackoff(3, base, maxDel, factor, func() float64 { return 0.9 })
	require.NotEqual(t, low, high,
		"equal-jitter must make the same attempt vary with the rand draw (fleet desync)")
	require.Less(t, low, high, "rand 0 gives the envelope floor, rand→1 approaches the ceiling")
}

// TestReconnectBackoffConfig_DefaultsAndClamp asserts the envelope resolution:
// zero values fall back to the defaults, and a reconnect_max_delay smaller than
// reconnect_delay is clamped UP to the base (a misconfiguration cannot invert
// the envelope).
func TestReconnectBackoffConfig_DefaultsAndClamp(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		s := &Session{opts: SessionOptions{}}
		base, maxDel := s.reconnectBackoffConfig()
		require.Equal(t, DefaultReconnectDelay, base)
		require.Equal(t, DefaultReconnectMaxDelay, maxDel)
	})
	t.Run("max_below_base_clamped_up", func(t *testing.T) {
		s := &Session{opts: SessionOptions{
			ReconnectDelay:    30 * time.Second,
			ReconnectMaxDelay: 1 * time.Second, // misconfigured: below the base
		}}
		base, maxDel := s.reconnectBackoffConfig()
		require.Equal(t, 30*time.Second, base)
		require.Equal(t, 30*time.Second, maxDel, "max is clamped up to the base, never below it")
	})
}

// TestNewReconnectBackoff_ComposesTakeoverPenalty asserts the composed autopaho
// backoff = jittered base + session-takeover penalty, using an injected rand so
// the assertion is exact and sleep-free.
func TestNewReconnectBackoff_ComposesTakeoverPenalty(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:        []string{"tcp://192.0.2.1:1883"},
		ClientID:          "backoff-compose",
		ReconnectDelay:    10 * time.Second,
		ReconnectMaxDelay: 2 * time.Minute,
	}, connectivity.SessionEphemeral, nil)

	// randFloat 0 pins the base delay to the envelope floor (d/2) so the sum
	// is exact. attempt 1 base d=10s → floor 5s.
	fn := s.newReconnectBackoff(func() float64 { return 0 })

	// No takeover in progress: penalty 0.
	require.Equal(t, 5*time.Second, fn(1), "attempt-1 floor delay with no takeover penalty")

	// Simulate an escalating takeover storm (streak 3 → penalty 1s<<1 = 2s).
	// The penalty is recency-gated, so an active storm must also carry a
	// RECENT takeover timestamp — a storm is precisely a run of takeovers still
	// arriving. Without it the penalty correctly decays to 0.
	s.mu.Lock()
	s.takeoverStreak = 3
	s.lastTakeoverAt = s.clock().Now().UnixNano()
	s.mu.Unlock()
	require.Equal(t, 5*time.Second+2*time.Second, fn(1),
		"the takeover penalty is added on top of the jittered base delay")

	// Attempt 0 stays 0 base, but the takeover penalty still applies (autopaho
	// only calls attempt 0 before the very first connect, where streak is 0 in
	// practice; this just pins the composition).
	require.Equal(t, 2*time.Second, fn(0))
}

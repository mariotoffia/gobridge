package persistence_test

import (
	"math"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// withinJitter checks that got is within ±25% of want.
func withinJitter(got, want time.Duration) bool {
	lo := time.Duration(float64(want) * 0.75)
	hi := time.Duration(float64(want) * 1.25)
	return got >= lo && got <= hi
}

// ═══════════════════════════════════════════════════════════════════════════
// FixedPoll Tests
//
// Verifies the constant-interval drain strategy that preserves
// backward-compatible polling behavior.
//
// ┌──────┬────────────────────────────────────────────────┬────────┐
// │ ID   │ Description                                    │ Type   │
// ├──────┼────────────────────────────────────────────────┼────────┤
// │ FP01 │ Returns configured interval regardless of args │ unit   │
// │ FP02 │ Zero interval defaults to 1s                   │ unit   │
// │ FP03 │ Negative interval defaults to 1s               │ unit   │
// │ FP04 │ Stable across repeated calls                   │ unit   │
// └──────┴────────────────────────────────────────────────┴────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestFixedPoll_NextInterval validates that FixedPoll returns the configured
// interval (±25% jitter) regardless of the recordsFound argument.
func TestFixedPoll_NextInterval(t *testing.T) {
	tests := []struct {
		name         string
		interval     time.Duration
		recordsFound int
		want         time.Duration
	}{
		{"zero records", 500 * time.Millisecond, 0, 500 * time.Millisecond},
		{"one record", 500 * time.Millisecond, 1, 500 * time.Millisecond},
		{"many records", 500 * time.Millisecond, 100, 500 * time.Millisecond},
		{"custom interval zero records", 2 * time.Second, 0, 2 * time.Second},
		{"custom interval many records", 2 * time.Second, 50, 2 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := persistence.NewFixedPoll(tt.interval)
			got := fp.NextInterval(tt.recordsFound)
			if !withinJitter(got, tt.want) {
				t.Errorf("NextInterval(%d) = %v, want %v ±25%%", tt.recordsFound, got, tt.want)
			}
		})
	}
}

// TestFixedPoll_DefaultInterval validates that zero or negative intervals
// fall back to DefaultFixedPollInterval (±25% jitter).
func TestFixedPoll_DefaultInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{"zero", 0},
		{"negative", -1 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := persistence.NewFixedPoll(tt.interval)
			got := fp.NextInterval(0)
			if !withinJitter(got, persistence.DefaultFixedPollInterval) {
				t.Errorf("NextInterval(0) = %v, want default %v ±25%%", got, persistence.DefaultFixedPollInterval)
			}
		})
	}
}

// TestFixedPoll_Stable validates that FixedPoll returns the configured
// interval (±25% jitter) across many consecutive calls with varying arguments.
func TestFixedPoll_Stable(t *testing.T) {
	fp := persistence.NewFixedPoll(250 * time.Millisecond)
	args := []int{0, 0, 5, 0, 100, 0, 0, 1}

	for i, n := range args {
		got := fp.NextInterval(n)
		if !withinJitter(got, 250*time.Millisecond) {
			t.Errorf("call %d: NextInterval(%d) = %v, want 250ms ±25%%", i, n, got)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// AdaptiveBackoff Tests
//
// Verifies the exponential backoff drain strategy that reduces polling
// cost when idle and resets to fast polling when records are found.
//
// State Machine:
//         ┌──────────────────────────────────────────┐
//         ▼                                          │
//     ○ MinInterval ──(0 records)──▶ current*mult    │
//         ▲                            │             │
//         │                            │ (0 records) │
//         │                            ▼             │
//         │                      current*mult        │
//         │                            │             │
//         │                            ▼             │
//         │                      MaxInterval (cap)   │
//         └──────── (records > 0) ───────────────────┘
//
// ┌──────┬────────────────────────────────────────────────┬────────┐
// │ ID   │ Description                                    │ Type   │
// ├──────┼────────────────────────────────────────────────┼────────┤
// │ AB01 │ Resets to MinInterval when records found       │ unit   │
// │ AB02 │ Backs off on empty results                     │ unit   │
// │ AB03 │ Caps at MaxInterval                            │ unit   │
// │ AB04 │ Full ramp from min to max                      │ unit   │
// │ AB05 │ Reset after backoff                            │ unit   │
// │ AB06 │ Constructor defaults for zero fields           │ unit   │
// │ AB07 │ Constructor clamps max < min                   │ unit   │
// │ AB08 │ Constructor enforces multiplier > 1.0          │ unit   │
// │ AB09 │ Reset() method restores MinInterval            │ unit   │
// └──────┴────────────────────────────────────────────────┴────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestAdaptiveBackoff_ResetOnRecords validates that NextInterval returns
// MinInterval (±25% jitter) whenever recordsFound > 0, regardless of prior backoff state.
func TestAdaptiveBackoff_ResetOnRecords(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, 2.0)

	// Back off several times to increase current interval.
	ab.NextInterval(0) // ~200ms
	ab.NextInterval(0) // ~400ms
	ab.NextInterval(0) // ~800ms

	got := ab.NextInterval(5)
	if !withinJitter(got, 100*time.Millisecond) {
		t.Errorf("after records found: got %v, want 100ms ±25%%", got)
	}
}

// TestAdaptiveBackoff_BackoffOnEmpty validates that NextInterval multiplies
// the current interval by Multiplier when no records are found (±25% jitter).
func TestAdaptiveBackoff_BackoffOnEmpty(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, 2.0)

	// Each base value doubles: 200, 400, 800, 1600.
	// With jitter, the returned value varies but the internal state
	// uses the non-jittered base for subsequent multiplications.
	bases := []time.Duration{
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
	}

	for i, base := range bases {
		got := ab.NextInterval(0)
		if !withinJitter(got, base) {
			t.Errorf("call %d: got %v, want %v ±25%%", i+1, got, base)
		}
	}
}

// TestAdaptiveBackoff_CapsAtMax validates that the backoff interval never
// exceeds MaxInterval + 25% jitter.
func TestAdaptiveBackoff_CapsAtMax(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(100*time.Millisecond, 500*time.Millisecond, 2.0)

	maxWithJitter := time.Duration(float64(500*time.Millisecond) * 1.25)

	// Ramp up past the cap.
	for i := range 20 {
		got := ab.NextInterval(0)
		if got > maxWithJitter {
			t.Fatalf("call %d: interval %v exceeds max 500ms + 25%% jitter", i+1, got)
		}
	}

	// After many calls, should be within jitter of the cap.
	got := ab.NextInterval(0)
	if !withinJitter(got, 500*time.Millisecond) {
		t.Errorf("expected capped near 500ms, got %v", got)
	}
}

// TestAdaptiveBackoff_FullRamp validates the complete backoff ramp from
// MinInterval to MaxInterval and back after reset.
//
// Timeline:
//
//	Call  recordsFound  Returned Interval
//	────────────────────────────────────────
//	1     0             200ms (100ms * 2)
//	2     0             400ms
//	3     0             800ms
//	4     0             1s (capped at 1s)
//	5     0             1s (stays capped)
//	6     3             100ms (reset)
//	7     0             200ms (backs off again)
func TestAdaptiveBackoff_FullRamp(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(100*time.Millisecond, 1*time.Second, 2.0)

	expected := []struct {
		records int
		want    time.Duration
	}{
		{0, 200 * time.Millisecond},
		{0, 400 * time.Millisecond},
		{0, 800 * time.Millisecond},
		{0, 1 * time.Second},
		{0, 1 * time.Second},
		{3, 100 * time.Millisecond},
		{0, 200 * time.Millisecond},
	}

	for i, tt := range expected {
		got := ab.NextInterval(tt.records)
		if !withinJitter(got, tt.want) {
			t.Errorf("step %d (records=%d): got %v, want %v ±25%%", i+1, tt.records, got, tt.want)
		}
	}
}

// TestAdaptiveBackoff_ConstructorDefaults validates that NewAdaptiveBackoff
// applies default values for zero-valued parameters.
func TestAdaptiveBackoff_ConstructorDefaults(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(0, 0, 0)

	if ab.MinInterval != persistence.DefaultAdaptiveMinInterval {
		t.Errorf("MinInterval = %v, want %v", ab.MinInterval, persistence.DefaultAdaptiveMinInterval)
	}
	if ab.MaxInterval != persistence.DefaultAdaptiveMaxInterval {
		t.Errorf("MaxInterval = %v, want %v", ab.MaxInterval, persistence.DefaultAdaptiveMaxInterval)
	}
	if ab.Multiplier != persistence.DefaultAdaptiveBackoffMultiplier {
		t.Errorf("Multiplier = %v, want %v", ab.Multiplier, persistence.DefaultAdaptiveBackoffMultiplier)
	}
}

// TestAdaptiveBackoff_MaxClamped validates that when maxInterval < minInterval,
// the constructor clamps maxInterval to minInterval.
func TestAdaptiveBackoff_MaxClamped(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(5*time.Second, 1*time.Second, 2.0)

	if ab.MaxInterval != 5*time.Second {
		t.Errorf("MaxInterval = %v, want 5s (clamped to MinInterval)", ab.MaxInterval)
	}

	got := ab.NextInterval(0)
	maxWithJitter := time.Duration(float64(5*time.Second) * 1.25)
	if got > maxWithJitter {
		t.Errorf("interval %v exceeds clamped max 5s + 25%% jitter", got)
	}
}

// TestAdaptiveBackoff_MultiplierFloor validates that multiplier <= 1.0
// is replaced with the default.
func TestAdaptiveBackoff_MultiplierFloor(t *testing.T) {
	tests := []struct {
		name       string
		multiplier float64
	}{
		{"zero", 0},
		{"one", 1.0},
		{"negative", -2.0},
		{"fraction", 0.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ab := persistence.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, tt.multiplier)
			if ab.Multiplier != persistence.DefaultAdaptiveBackoffMultiplier {
				t.Errorf("Multiplier = %v, want default %v", ab.Multiplier, persistence.DefaultAdaptiveBackoffMultiplier)
			}
		})
	}
}

// TestAdaptiveBackoff_Reset validates that the Reset method restores
// the internal state to MinInterval.
func TestAdaptiveBackoff_Reset(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, 2.0)

	ab.NextInterval(0) // ~200ms
	ab.NextInterval(0) // ~400ms
	ab.NextInterval(0) // ~800ms

	ab.Reset()

	got := ab.NextInterval(0)
	if !withinJitter(got, 200*time.Millisecond) {
		t.Errorf("after Reset + empty: got %v, want 200ms ±25%% (min * multiplier)", got)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Jitter overflow regression
//
// A parseable-but-absurd interval (e.g. a duration near math.MaxInt64) must
// never make applyJitter overflow time.Duration to a NEGATIVE value: a
// negative interval makes Clock.After/time.After fire immediately, so an idle
// drainer spins hot instead of backing off. Both strategies must saturate.
// ═══════════════════════════════════════════════════════════════════════════

// TestFixedPoll_MaxDurationNoOverflow proves FixedPoll never returns a
// negative (overflowed) interval even when constructed with the maximum
// representable duration. Runs many iterations because the jitter sign is
// random; a broken +25% would overflow on any positive-jitter draw.
func TestFixedPoll_MaxDurationNoOverflow(t *testing.T) {
	fp := persistence.NewFixedPoll(time.Duration(math.MaxInt64))
	for i := 0; i < 2000; i++ {
		if got := fp.NextInterval(0); got < 0 {
			t.Fatalf("iter %d: NextInterval returned negative interval %v (jitter overflow)", i, got)
		}
	}
}

// TestAdaptiveBackoff_MaxDurationNoOverflow proves AdaptiveBackoff never
// returns a negative (overflowed) interval when constructed with max-duration
// bounds AND an enormous multiplier: the backoff multiply and the jitter both
// have to saturate rather than wrap. Exercises both the empty-poll (backoff)
// and records-found (reset) paths.
func TestAdaptiveBackoff_MaxDurationNoOverflow(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(time.Duration(math.MaxInt64), time.Duration(math.MaxInt64), 1e18)
	for i := 0; i < 2000; i++ {
		if got := ab.NextInterval(0); got < 0 {
			t.Fatalf("iter %d: backoff NextInterval returned negative interval %v (overflow)", i, got)
		}
		if got := ab.NextInterval(1); got < 0 {
			t.Fatalf("iter %d: reset NextInterval returned negative interval %v (overflow)", i, got)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Jitter low-end truncation regression
//
// A sub-4ns interval must never be jittered DOWN to zero: for d = 1ns the
// jittered float (0.75..1.25) truncates toward zero on the time.Duration
// conversion, yielding 0 roughly half the time. Clock.After(0) fires
// immediately, so a drainer configured with a tiny poll interval spins hot at
// the LOW end exactly like the overflow case does at the high end. applyJitter
// must floor any positive-input result at a single nanosecond.
// ═══════════════════════════════════════════════════════════════════════════

// TestFixedPoll_SubResolutionNeverZero proves a 1ns FixedPoll never yields a
// zero interval. Without the low-end floor roughly half of these draws
// truncate to time.Duration(0); the loop count makes a regression virtually
// certain to trip.
func TestFixedPoll_SubResolutionNeverZero(t *testing.T) {
	fp := persistence.NewFixedPoll(1 * time.Nanosecond)
	for i := 0; i < 2000; i++ {
		if got := fp.NextInterval(0); got < time.Nanosecond {
			t.Fatalf("iter %d: NextInterval returned %v, want >= 1ns (low-end truncation to zero hot-spins)", i, got)
		}
	}
}

// TestAdaptiveBackoff_SubResolutionNeverZero proves the adaptive strategy never
// yields a zero interval at sub-nanosecond-adjacent bounds, on BOTH the
// records-found reset path (min = 1ns) and the empty-poll backoff path.
func TestAdaptiveBackoff_SubResolutionNeverZero(t *testing.T) {
	ab := persistence.NewAdaptiveBackoff(1*time.Nanosecond, 4*time.Nanosecond, 2.0)
	for i := 0; i < 2000; i++ {
		if got := ab.NextInterval(1); got < time.Nanosecond { // reset -> applyJitter(1ns)
			t.Fatalf("iter %d: reset NextInterval returned %v, want >= 1ns", i, got)
		}
		if got := ab.NextInterval(0); got < time.Nanosecond { // backoff -> applyJitter(small)
			t.Fatalf("iter %d: backoff NextInterval returned %v, want >= 1ns", i, got)
		}
	}
}

// TestAdaptiveBackoff_NonFiniteMultiplier proves a non-finite multiplier is
// rejected at construction and replaced with the default. NaN in particular
// slips past a bare `multiplier <= 1.0` (every NaN comparison is false); left
// unguarded, NextInterval would convert float64(current)*NaN into a garbage
// (typically hugely negative) time.Duration and hot-spin the drainer. +Inf and
// -Inf are rejected too. Each case must also produce a sane, positive interval.
func TestAdaptiveBackoff_NonFiniteMultiplier(t *testing.T) {
	tests := []struct {
		name       string
		multiplier float64
	}{
		{"nan", math.NaN()},
		{"pos_inf", math.Inf(1)},
		{"neg_inf", math.Inf(-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ab := persistence.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, tt.multiplier)
			if ab.Multiplier != persistence.DefaultAdaptiveBackoffMultiplier {
				t.Fatalf("Multiplier = %v, want default %v", ab.Multiplier, persistence.DefaultAdaptiveBackoffMultiplier)
			}
			// End-to-end: the backoff path must stay positive rather than
			// converting a non-finite product into a negative duration.
			for i := 0; i < 8; i++ {
				if got := ab.NextInterval(0); got <= 0 {
					t.Fatalf("iter %d: NextInterval returned non-positive %v with %s multiplier", i, got, tt.name)
				}
			}
		})
	}
}

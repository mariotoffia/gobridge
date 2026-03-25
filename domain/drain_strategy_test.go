package domain_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

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
// interval regardless of the recordsFound argument.
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
			fp := domain.NewFixedPoll(tt.interval)
			got := fp.NextInterval(tt.recordsFound)
			if got != tt.want {
				t.Errorf("NextInterval(%d) = %v, want %v", tt.recordsFound, got, tt.want)
			}
		})
	}
}

// TestFixedPoll_DefaultInterval validates that zero or negative intervals
// fall back to DefaultFixedPollInterval.
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
			fp := domain.NewFixedPoll(tt.interval)
			got := fp.NextInterval(0)
			if got != domain.DefaultFixedPollInterval {
				t.Errorf("NextInterval(0) = %v, want default %v", got, domain.DefaultFixedPollInterval)
			}
		})
	}
}

// TestFixedPoll_Stable validates that FixedPoll returns the same interval
// across many consecutive calls with varying arguments.
func TestFixedPoll_Stable(t *testing.T) {
	fp := domain.NewFixedPoll(250 * time.Millisecond)
	args := []int{0, 0, 5, 0, 100, 0, 0, 1}

	for i, n := range args {
		got := fp.NextInterval(n)
		if got != 250*time.Millisecond {
			t.Errorf("call %d: NextInterval(%d) = %v, want 250ms", i, n, got)
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
// MinInterval whenever recordsFound > 0, regardless of prior backoff state.
func TestAdaptiveBackoff_ResetOnRecords(t *testing.T) {
	ab := domain.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, 2.0)

	// Back off several times to increase current interval.
	ab.NextInterval(0) // 200ms
	ab.NextInterval(0) // 400ms
	ab.NextInterval(0) // 800ms

	got := ab.NextInterval(5)
	if got != 100*time.Millisecond {
		t.Errorf("after records found: got %v, want 100ms", got)
	}
}

// TestAdaptiveBackoff_BackoffOnEmpty validates that NextInterval multiplies
// the current interval by Multiplier when no records are found.
func TestAdaptiveBackoff_BackoffOnEmpty(t *testing.T) {
	ab := domain.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, 2.0)

	tests := []struct {
		call int
		want time.Duration
	}{
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{4, 1600 * time.Millisecond},
	}

	for _, tt := range tests {
		got := ab.NextInterval(0)
		if got != tt.want {
			t.Errorf("call %d: got %v, want %v", tt.call, got, tt.want)
		}
	}
}

// TestAdaptiveBackoff_CapsAtMax validates that the backoff interval never
// exceeds MaxInterval.
func TestAdaptiveBackoff_CapsAtMax(t *testing.T) {
	ab := domain.NewAdaptiveBackoff(100*time.Millisecond, 500*time.Millisecond, 2.0)

	// Ramp up past the cap.
	for i := 0; i < 20; i++ {
		got := ab.NextInterval(0)
		if got > 500*time.Millisecond {
			t.Fatalf("call %d: interval %v exceeds max 500ms", i+1, got)
		}
	}

	// After many calls, should be exactly at the cap.
	got := ab.NextInterval(0)
	if got != 500*time.Millisecond {
		t.Errorf("expected capped at 500ms, got %v", got)
	}
}

// TestAdaptiveBackoff_FullRamp validates the complete backoff ramp from
// MinInterval to MaxInterval and back after reset.
//
// Timeline:
//   Call  recordsFound  Returned Interval
//   ────────────────────────────────────────
//   1     0             200ms (100ms * 2)
//   2     0             400ms
//   3     0             800ms
//   4     0             1s (capped at 1s)
//   5     0             1s (stays capped)
//   6     3             100ms (reset)
//   7     0             200ms (backs off again)
func TestAdaptiveBackoff_FullRamp(t *testing.T) {
	ab := domain.NewAdaptiveBackoff(100*time.Millisecond, 1*time.Second, 2.0)

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
		if got != tt.want {
			t.Errorf("step %d (records=%d): got %v, want %v", i+1, tt.records, got, tt.want)
		}
	}
}

// TestAdaptiveBackoff_ConstructorDefaults validates that NewAdaptiveBackoff
// applies default values for zero-valued parameters.
func TestAdaptiveBackoff_ConstructorDefaults(t *testing.T) {
	ab := domain.NewAdaptiveBackoff(0, 0, 0)

	if ab.MinInterval != domain.DefaultAdaptiveMinInterval {
		t.Errorf("MinInterval = %v, want %v", ab.MinInterval, domain.DefaultAdaptiveMinInterval)
	}
	if ab.MaxInterval != domain.DefaultAdaptiveMaxInterval {
		t.Errorf("MaxInterval = %v, want %v", ab.MaxInterval, domain.DefaultAdaptiveMaxInterval)
	}
	if ab.Multiplier != domain.DefaultAdaptiveBackoffMultiplier {
		t.Errorf("Multiplier = %v, want %v", ab.Multiplier, domain.DefaultAdaptiveBackoffMultiplier)
	}
}

// TestAdaptiveBackoff_MaxClamped validates that when maxInterval < minInterval,
// the constructor clamps maxInterval to minInterval.
func TestAdaptiveBackoff_MaxClamped(t *testing.T) {
	ab := domain.NewAdaptiveBackoff(5*time.Second, 1*time.Second, 2.0)

	if ab.MaxInterval != 5*time.Second {
		t.Errorf("MaxInterval = %v, want 5s (clamped to MinInterval)", ab.MaxInterval)
	}

	got := ab.NextInterval(0)
	if got > 5*time.Second {
		t.Errorf("interval %v exceeds clamped max 5s", got)
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
			ab := domain.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, tt.multiplier)
			if ab.Multiplier != domain.DefaultAdaptiveBackoffMultiplier {
				t.Errorf("Multiplier = %v, want default %v", ab.Multiplier, domain.DefaultAdaptiveBackoffMultiplier)
			}
		})
	}
}

// TestAdaptiveBackoff_Reset validates that the Reset method restores
// the internal state to MinInterval.
func TestAdaptiveBackoff_Reset(t *testing.T) {
	ab := domain.NewAdaptiveBackoff(100*time.Millisecond, 10*time.Second, 2.0)

	ab.NextInterval(0) // 200ms
	ab.NextInterval(0) // 400ms
	ab.NextInterval(0) // 800ms

	ab.Reset()

	got := ab.NextInterval(0)
	if got != 200*time.Millisecond {
		t.Errorf("after Reset + empty: got %v, want 200ms (min * multiplier)", got)
	}
}

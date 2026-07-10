package persistence

import (
	"math"
	"math/rand/v2"
	"time"
)

// DrainStrategy determines the polling interval between outbox drain cycles.
// The OutboxDrainer calls NextInterval after each cycle, passing the number
// of records that were drained, and uses the returned duration as the wait
// before the next cycle.
type DrainStrategy interface {
	NextInterval(recordsFound int) time.Duration
}

// Default drain strategy values.
const (
	DefaultFixedPollInterval         = 1 * time.Second
	DefaultAdaptiveMinInterval       = 100 * time.Millisecond
	DefaultAdaptiveMaxInterval       = 30 * time.Second
	DefaultAdaptiveBackoffMultiplier = 2.0
)

// maxDrainInterval caps an accepted drain interval so that the +25% jitter
// applied by applyJitter can never overflow time.Duration (int64 ns). At the
// cap, 1.25 x maxDrainInterval stays at or below math.MaxInt64. It equals
// math.MaxInt64/1.25, expressed with integer math to avoid float-constant
// conversion pitfalls (~234 years, far above any real poll interval).
const maxDrainInterval = time.Duration(math.MaxInt64 / 5 * 4)

// FixedPoll is a DrainStrategy that always returns the same interval
// regardless of whether records were found. This is the default strategy
// and preserves backward-compatible behavior.
type FixedPoll struct {
	Interval time.Duration
}

// NewFixedPoll creates a FixedPoll strategy. If interval is zero,
// DefaultFixedPollInterval is used.
func NewFixedPoll(interval time.Duration) *FixedPoll {
	if interval <= 0 {
		interval = DefaultFixedPollInterval
	}
	if interval > maxDrainInterval {
		interval = maxDrainInterval
	}
	return &FixedPoll{Interval: interval}
}

// NextInterval returns the configured interval with ±25% jitter to
// prevent thundering herd when multiple instances poll concurrently.
func (f *FixedPoll) NextInterval(_ int) time.Duration {
	return applyJitter(f.Interval)
}

// AdaptiveBackoff is a DrainStrategy that uses exponential backoff when
// the outbox is empty and resets to MinInterval when records are found.
// This reduces DynamoDB read cost during idle periods while maintaining
// low latency when messages are flowing.
//
// AdaptiveBackoff is NOT safe for concurrent use. It is intended to be
// called from a single goroutine (the OutboxDrainer.Run loop).
type AdaptiveBackoff struct {
	MinInterval time.Duration
	MaxInterval time.Duration
	Multiplier  float64
	current     time.Duration
}

// NewAdaptiveBackoff creates an AdaptiveBackoff strategy with the given
// parameters. Zero-valued fields are replaced with defaults.
func NewAdaptiveBackoff(minInterval, maxInterval time.Duration, multiplier float64) *AdaptiveBackoff {
	if minInterval <= 0 {
		minInterval = DefaultAdaptiveMinInterval
	}
	if maxInterval <= 0 {
		maxInterval = DefaultAdaptiveMaxInterval
	}
	// Reject a non-finite or non-amplifying multiplier. NaN slips past a
	// bare `multiplier <= 1.0` (every NaN comparison is false), and NextInterval
	// would then convert float64(current)*NaN into a garbage (typically hugely
	// negative) time.Duration, making Clock.After fire immediately and hot-spin
	// the drainer. +Inf is caught here too rather than relying on the downstream
	// MaxInterval saturation. -Inf and any value <= 1.0 fall through to the same
	// default.
	if math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier <= 1.0 {
		multiplier = DefaultAdaptiveBackoffMultiplier
	}
	// Cap both bounds so applyJitter's +25% can never overflow time.Duration
	// (see maxDrainInterval / applyJitter). Apply before the max<min clamp so
	// a capped min still governs a smaller-but-uncapped max.
	if minInterval > maxDrainInterval {
		minInterval = maxDrainInterval
	}
	if maxInterval > maxDrainInterval {
		maxInterval = maxDrainInterval
	}
	if maxInterval < minInterval {
		maxInterval = minInterval
	}
	return &AdaptiveBackoff{
		MinInterval: minInterval,
		MaxInterval: maxInterval,
		Multiplier:  multiplier,
		current:     minInterval,
	}
}

// NextInterval returns MinInterval (with jitter) when records were found
// (fast poll), otherwise multiplies the current interval by Multiplier,
// capped at MaxInterval, with ±25% jitter to prevent thundering herd.
func (a *AdaptiveBackoff) NextInterval(recordsFound int) time.Duration {
	if recordsFound > 0 {
		a.current = a.MinInterval
		return applyJitter(a.current)
	}

	// Compute the next backoff in float64 and saturate at MaxInterval so a
	// large Multiplier or current value can never overflow time.Duration
	// (int64 ns) to a negative interval, which would make Clock.After fire
	// immediately and hot-spin the drainer.
	next := float64(a.current) * a.Multiplier
	if next >= float64(a.MaxInterval) {
		a.current = a.MaxInterval
	} else {
		a.current = time.Duration(next)
	}
	return applyJitter(a.current)
}

// Reset restores the adaptive backoff to its initial state (MinInterval).
func (a *AdaptiveBackoff) Reset() {
	a.current = a.MinInterval
}

// applyJitter adds ±25% random jitter to a duration, matching the
// pattern used by the SQS receiver poll backoff. This prevents
// synchronized polling across multiple instances (thundering herd).
//
// For any positive input the result is guaranteed to be in
// [time.Nanosecond, math.MaxInt64]: a WAIT was requested, so the drainer must
// never be handed a zero or negative interval that makes Clock.After fire
// immediately and hot-spin.
func applyJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// Compute and saturate the jittered value entirely in float64, then clamp
	// BEFORE converting back to time.Duration.
	//
	// High end: a large d (a parseable-but-absurd interval) could otherwise
	// overflow the int64 nanosecond representation to a NEGATIVE interval.
	//
	// Low end: for a sub-4ns d the jittered float (0.75d..1.25d) truncates
	// toward zero on the time.Duration conversion — d = 1ns yields 0.75..1.25,
	// which becomes time.Duration(0) roughly half the time. A zero interval
	// hot-spins the drainer exactly like a negative one, so floor the result at
	// a single nanosecond (d > 0 here, so the caller asked to wait).
	jittered := float64(d) + float64(d)*0.25*(2*rand.Float64()-1)
	if jittered >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	result := time.Duration(jittered)
	if result < time.Nanosecond {
		result = time.Nanosecond
	}
	return result
}

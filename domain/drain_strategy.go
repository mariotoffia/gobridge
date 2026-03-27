package domain

import "time"

// DrainStrategy determines the polling interval between outbox drain cycles.
// The OutboxDrainer calls NextInterval after each cycle, passing the number
// of records that were drained, and uses the returned duration as the wait
// before the next cycle.
type DrainStrategy interface {
	NextInterval(recordsFound int) time.Duration
}

// Default drain strategy values.
var (
	DefaultFixedPollInterval        = 1 * time.Second
	DefaultAdaptiveMinInterval      = 100 * time.Millisecond
	DefaultAdaptiveMaxInterval      = 30 * time.Second
	DefaultAdaptiveBackoffMultiplier = 2.0
)

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
	return &FixedPoll{Interval: interval}
}

// NextInterval always returns the configured interval.
func (f *FixedPoll) NextInterval(_ int) time.Duration {
	return f.Interval
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
	if multiplier <= 1.0 {
		multiplier = DefaultAdaptiveBackoffMultiplier
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

// NextInterval returns MinInterval when records were found (fast poll),
// otherwise multiplies the current interval by Multiplier, capped at
// MaxInterval.
func (a *AdaptiveBackoff) NextInterval(recordsFound int) time.Duration {
	if recordsFound > 0 {
		a.current = a.MinInterval
		return a.current
	}

	next := time.Duration(float64(a.current) * a.Multiplier)
	if next > a.MaxInterval {
		next = a.MaxInterval
	}
	a.current = next
	return next
}

// Reset restores the adaptive backoff to its initial state (MinInterval).
func (a *AdaptiveBackoff) Reset() {
	a.current = a.MinInterval
}

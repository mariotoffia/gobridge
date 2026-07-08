package sqs

import (
	"testing"
	"time"
)

func jitterBand(base time.Duration) (low, high time.Duration) {
	low = time.Duration(float64(base) * 0.75)
	high = time.Duration(float64(base) * 1.25)
	return low, high
}

func defaultBackoffConfig() ReceiverConfig {
	cfg := ReceiverConfig{}
	cfg.applyDefaults()
	return cfg
}

// TestPollBackoffNextInitialS10 verifies the first next() delay is near the
// configured initial delay within the jitter window.
func TestPollBackoffNextInitialS10(t *testing.T) {
	t.Parallel()

	cfg := defaultBackoffConfig()
	b := newPollBackoffFromConfig(cfg)
	low, high := jitterBand(cfg.PollBackoffInitial)
	d := b.next()
	if d < low || d > high {
		t.Fatalf("first delay %v want within [%v, %v]", d, low, high)
	}
}

// TestPollBackoffDoublingAndCapS10 verifies exponential growth of the base
// delay (1s -> 2s -> 4s -> 8s -> 16s -> 30s cap) via jittered return ranges.
func TestPollBackoffDoublingAndCapS10(t *testing.T) {
	t.Parallel()

	cfg := defaultBackoffConfig()
	bases := []time.Duration{
		cfg.PollBackoffInitial,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		cfg.PollBackoffMax,
		cfg.PollBackoffMax,
	}

	b := newPollBackoffFromConfig(cfg)
	for i, base := range bases {
		low, high := jitterBand(base)
		d := b.next()
		if d < low || d > high {
			t.Fatalf("step %d: delay %v want within [%v, %v] (base %v)", i, d, low, high, base)
		}
	}
}

// TestPollBackoffJitterNeverExceedsMaxS10 is the regression for Finding 11:
// jitter was added AFTER the base was capped, so a +25% draw could return up
// to 1.25*PollBackoffMax — violating the operator contract that a retry never
// waits longer than PollBackoffMax. Once saturated, every jittered draw must
// stay at or below the cap. Many draws exercise the random +25% path.
func TestPollBackoffJitterNeverExceedsMaxS10(t *testing.T) {
	t.Parallel()

	cfg := defaultBackoffConfig()
	b := newPollBackoffFromConfig(cfg)

	// Saturate the base to the ceiling.
	for i := 0; i < 20; i++ {
		_ = b.next()
	}

	for i := 0; i < 10000; i++ {
		if d := b.next(); d > cfg.PollBackoffMax {
			t.Fatalf("draw %d: delay %v exceeds cap %v (Finding 11: jitter over cap)", i, d, cfg.PollBackoffMax)
		}
	}
}

// TestPollBackoffResetS10 verifies reset restores the backoff to the initial
// scale on the following next() call.
func TestPollBackoffResetS10(t *testing.T) {
	t.Parallel()

	cfg := defaultBackoffConfig()
	b := newPollBackoffFromConfig(cfg)
	_ = b.next()
	_ = b.next()
	b.reset()

	low, high := jitterBand(cfg.PollBackoffInitial)
	d := b.next()
	if d < low || d > high {
		t.Fatalf("after reset delay %v want within [%v, %v]", d, low, high)
	}
}

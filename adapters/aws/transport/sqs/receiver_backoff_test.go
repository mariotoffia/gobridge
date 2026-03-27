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

// TestPollBackoffNextInitialS10 verifies the first next() delay is near 1s
// within the jitter window.
func TestPollBackoffNextInitialS10(t *testing.T) {
	t.Parallel()

	b := newPollBackoff()
	low, high := jitterBand(pollBackoffInitial)
	d := b.next()
	if d < low || d > high {
		t.Fatalf("first delay %v want within [%v, %v]", d, low, high)
	}
}

// TestPollBackoffDoublingAndCapS10 verifies exponential growth of the base
// delay (1s -> 2s -> 4s -> 8s -> 16s -> 30s cap) via jittered return ranges.
func TestPollBackoffDoublingAndCapS10(t *testing.T) {
	t.Parallel()

	bases := []time.Duration{
		pollBackoffInitial,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		pollBackoffMax,
		pollBackoffMax,
	}

	b := newPollBackoff()
	for i, base := range bases {
		low, high := jitterBand(base)
		d := b.next()
		if d < low || d > high {
			t.Fatalf("step %d: delay %v want within [%v, %v] (base %v)", i, d, low, high, base)
		}
	}
}

// TestPollBackoffResetS10 verifies reset restores the backoff to the initial
// 1s scale on the following next() call.
func TestPollBackoffResetS10(t *testing.T) {
	t.Parallel()

	b := newPollBackoff()
	_ = b.next()
	_ = b.next()
	b.reset()

	low, high := jitterBand(pollBackoffInitial)
	d := b.next()
	if d < low || d > high {
		t.Fatalf("after reset delay %v want within [%v, %v]", d, low, high)
	}
}

package amqp10

import (
	"testing"
	"time"
)

func TestBackoff_Initial(t *testing.T) {
	// verifies newBackoff starts at the initial delay of 1s
	b := newBackoff()
	if b.current != backoffInitial {
		t.Fatalf("initial current = %v, want %v", b.current, backoffInitial)
	}
}

func TestBackoff_Increases(t *testing.T) {
	// verifies successive next() calls produce increasing delays
	b := newBackoff()

	d1 := b.next()
	d2 := b.next()
	d3 := b.next()

	// Strip jitter by checking the internal state progression.
	// After first next(): current = 1s * 2 = 2s
	// After second next(): current = 2s * 2 = 4s
	// After third next(): current = 4s * 2 = 8s
	// The returned values include jitter but should be in the right ballpark.
	if d2 <= d1/2 {
		t.Fatalf("expected d2 (%v) > d1/2 (%v); delays should increase", d2, d1/2)
	}
	if d3 <= d2/2 {
		t.Fatalf("expected d3 (%v) > d2/2 (%v); delays should increase", d3, d2/2)
	}
}

func TestBackoff_CapsAtMax(t *testing.T) {
	// verifies the delay never exceeds backoffMax (30s)
	b := newBackoff()

	for i := 0; i < 20; i++ {
		d := b.next()
		maxWithJitter := backoffMax + time.Duration(float64(backoffMax)*0.25)
		if d > maxWithJitter {
			t.Fatalf("next() = %v on iteration %d, exceeds max+jitter %v", d, i, maxWithJitter)
		}
	}

	if b.current > backoffMax {
		t.Fatalf("internal current = %v, should be capped at %v", b.current, backoffMax)
	}
}

func TestBackoff_Jitter(t *testing.T) {
	// verifies the returned delay includes jitter within ±25% of the base
	b := newBackoff()

	seen := make(map[time.Duration]bool)
	for i := 0; i < 50; i++ {
		b.reset()
		d := b.next()
		seen[d] = true

		low := time.Duration(float64(backoffInitial) * 0.75)
		high := time.Duration(float64(backoffInitial) * 1.25)
		if d < low || d > high {
			t.Fatalf("next() = %v, want within [%v, %v]", d, low, high)
		}
	}

	if len(seen) < 2 {
		t.Fatalf("expected jitter variation, but got %d distinct values out of 50 calls", len(seen))
	}
}

func TestBackoff_Reset(t *testing.T) {
	// verifies reset() returns the backoff to the initial delay
	b := newBackoff()

	b.next()
	b.next()
	b.next()

	b.reset()

	if b.current != backoffInitial {
		t.Fatalf("current after reset = %v, want %v", b.current, backoffInitial)
	}

	d := b.next()
	low := time.Duration(float64(backoffInitial) * 0.75)
	high := time.Duration(float64(backoffInitial) * 1.25)
	if d < low || d > high {
		t.Fatalf("next() after reset = %v, want within [%v, %v]", d, low, high)
	}
}

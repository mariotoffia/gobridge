package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
)

// TestWatchTerminal_TransientPositiveDoesNotExit reproduces
// backstop hardening: a single (or a few isolated) transient positive terminal
// reads — exactly what a swap window can produce — must NOT trip the process
// exit. Only terminalConfirmSamples CONSECUTIVE positives may. Here the
// predicate returns true only on isolated samples, so the required run of
// consecutive positives is never reached and watchTerminal returns false when
// the context is cancelled.
func TestWatchTerminal_TransientPositiveDoesNotExit(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pred := func() bool {
		n := calls.Add(1)
		if n >= 20 {
			cancel() // end the test after enough samples
		}
		// Isolated transient positives separated by resets — never
		// terminalConfirmSamples in a row.
		return n == 3 || n == 8 || n == 13
	}

	done := make(chan bool, 1)
	go func() {
		done <- watchTerminal(ctx, clock.System, time.Millisecond, pred)
	}()

	select {
	case got := <-done:
		if got {
			t.Fatal("watchTerminal must NOT exit on isolated transient positives")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchTerminal did not return")
	}
}

// TestWatchTerminal_RequiresConsecutivePositives verifies the counter resets on
// any negative sample: a transient positive followed by negatives, then a
// SUSTAINED run of positives, exits only once terminalConfirmSamples positives
// occur back-to-back (a genuinely wedged supervisor).
func TestWatchTerminal_RequiresConsecutivePositives(t *testing.T) {
	var calls atomic.Int32
	pred := func() bool {
		n := calls.Add(1)
		// One transient positive, a reset, then sustained-terminal.
		if n == 2 {
			return true
		}
		return n >= 5
	}

	got := watchTerminal(context.Background(), clock.System, time.Millisecond, pred)
	if !got {
		t.Fatal("watchTerminal must exit once terminalConfirmSamples consecutive positives are seen")
	}
	// Sustained positives begin at call 5; three consecutive → exit at call 7.
	// Assert the EXACT count: an off-by-one impl requiring 4 consecutive would
	// exit at call 8 and still satisfy a `>= 7` bound, so pin it precisely.
	if c := calls.Load(); c != 7 {
		t.Fatalf("expected exactly 7 polls before exit (reset + 3 consecutive), got %d", c)
	}
}

package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
)

// TestWatchTerminal_ReturnsTrueWhenTerminalObserved proves the backstop keeps
// polling and reports terminal once the predicate flips — not a one-shot check.
func TestWatchTerminal_ReturnsTrueWhenTerminalObserved(t *testing.T) {
	calls := 0
	got := watchTerminal(context.Background(), clock.System, time.Millisecond, func() bool {
		calls++
		return calls >= 3
	})

	if !got {
		t.Fatal("watchTerminal should return true once the runtime is terminal")
	}
	if calls < 3 {
		t.Fatalf("expected the predicate to be polled repeatedly, got %d calls", calls)
	}
}

// TestWatchTerminal_ReturnsFalseOnContextCancel proves a non-terminal runtime
// lets the watcher unwind cleanly on shutdown instead of forcing an exit.
func TestWatchTerminal_ReturnsFalseOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- watchTerminal(ctx, clock.System, time.Millisecond, func() bool { return false })
	}()

	cancel()

	select {
	case got := <-done:
		if got {
			t.Fatal("watchTerminal must return false when ctx is cancelled before terminal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchTerminal did not return after ctx cancel")
	}
}

// discardLogger returns a logger that drops output — awaitSupervisorShutdown's
// behaviour is verified by its return timing, not by what it logs.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runsWithin runs fn in a goroutine and reports whether it returned within d.
func runsWithin(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestAwaitSupervisorShutdown_SkipsWaitWhenSupervisorAlreadyExited is the C3-FU5
// regression guard: once the primary select has consumed the supervisor's only
// result, the shutdown wait must return immediately. Both channels here never
// deliver, so a return can only come from the alreadyExited fast path — the old
// code would have blocked reading the already-drained supDone until the deadline.
func TestAwaitSupervisorShutdown_SkipsWaitWhenSupervisorAlreadyExited(t *testing.T) {
	supDone := make(chan error) // never receives
	done := make(chan struct{}) // never fires (stands in for the shutdown deadline)

	if !runsWithin(2*time.Second, func() {
		awaitSupervisorShutdown(true, supDone, done, discardLogger())
	}) {
		t.Fatal("awaitSupervisorShutdown must return immediately when the supervisor already exited")
	}
}

// TestAwaitSupervisorShutdown_WaitsForSupervisorToUnwind proves the normal
// (signal/terminal) path is preserved: with the supervisor still running, the
// helper blocks on supDone and returns once it reports it has stopped.
func TestAwaitSupervisorShutdown_WaitsForSupervisorToUnwind(t *testing.T) {
	supDone := make(chan error, 1)
	supDone <- nil // supervisor unwinds cleanly after ctx cancel

	if !runsWithin(2*time.Second, func() {
		awaitSupervisorShutdown(false, supDone, make(chan struct{}), discardLogger())
	}) {
		t.Fatal("awaitSupervisorShutdown should return once the supervisor reports it stopped")
	}
}

// TestAwaitSupervisorShutdown_ReturnsOnDeadline proves the bounded wait still
// unblocks on the shutdown deadline when a running supervisor fails to unwind.
func TestAwaitSupervisorShutdown_ReturnsOnDeadline(t *testing.T) {
	deadline := make(chan struct{})
	close(deadline) // deadline already elapsed

	if !runsWithin(2*time.Second, func() {
		awaitSupervisorShutdown(false, make(chan error), deadline, discardLogger())
	}) {
		t.Fatal("awaitSupervisorShutdown should return when the shutdown deadline fires")
	}
}

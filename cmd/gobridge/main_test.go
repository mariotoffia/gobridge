package main

import (
	"context"
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

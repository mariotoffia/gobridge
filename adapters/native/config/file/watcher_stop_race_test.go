package file

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// TestWatcher_StopBlocksUntilLoopExits is the Finding 8 regression: Stop must
// wait for the watch loop to fully exit before returning, so a rapid
// Stop-then-Watch cycle never leaves the old loop alive alongside a new one,
// both mutating lastHash without a lock. Pre-fix Stop flipped running=false
// and returned immediately; the next Watch could then start a second loop
// while the first was still draining.
//
// Determinism: after Stop returns, the change channel is CLOSED — the loop ran
// its deferred teardown (which closes the channel) and only then signalled the
// done channel Stop blocks on. An orphaned loop would leave the channel open.
// Runs under -race, so a genuine two-loop overlap is also flagged.
func TestWatcher_StopBlocksUntilLoopExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "initial")

	w := NewWatcher(path, newTestRegistry(t))

	for i := 0; i < 25; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := w.Watch(ctx)
		if err != nil {
			cancel()
			t.Fatalf("cycle %d: Watch failed (previous loop not fully stopped): %v", i, err)
		}
		<-w.Started()

		w.Stop()

		if !channelClosed(ch) {
			cancel()
			t.Fatalf("cycle %d: change channel still open after Stop returned; loop was orphaned", i)
		}
		cancel()
	}
}

// TestWatcher_ConcurrentStop_NoDoubleClose is the regression for the
// concurrent-Stop double close: running is only cleared by the loop's deferred
// teardown, so two goroutines calling Stop at once both passed the running
// guard and both closed stopCh — a second close of a channel panics. The
// stopping guard makes the close happen at most once while every caller still
// blocks on the done channel. Runs under -race; a regression panics the test.
func TestWatcher_ConcurrentStop_NoDoubleClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	writeYAML(t, path, "initial")

	w := NewWatcher(path, newTestRegistry(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := w.Watch(ctx); err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	<-w.Started()

	const stoppers = 8
	var wg sync.WaitGroup
	wg.Add(stoppers)
	start := make(chan struct{})
	for i := 0; i < stoppers; i++ {
		go func() {
			defer wg.Done()
			<-start // release all at once to widen the race window
			w.Stop()
		}()
	}
	close(start)
	wg.Wait() // a double close would have panicked before we reach here

	// The watcher restarts cleanly: Watch re-arms the stopping guard.
	if _, err := w.Watch(ctx); err != nil {
		t.Fatalf("Watch after concurrent Stop failed: %v", err)
	}
	<-w.Started()
	w.Stop()
}

// channelClosed reports whether ch is closed, draining a single pending
// buffered value first if present. It never blocks.
func channelClosed(ch <-chan *ports.BridgeConfig) bool {
	select {
	case _, ok := <-ch:
		if !ok {
			return true
		}
	default:
		return false
	}
	// Drained one buffered value; the next read must observe the close.
	select {
	case _, ok := <-ch:
		return !ok
	default:
		return false
	}
}

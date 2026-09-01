package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
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

// TestNewDefaultCredentialResolver_FileStoreInitFailure_DoesNotAbort validates
// adversarial Finding 2: when the native file:// store cannot initialize, the
// stock resolver is still returned so a config that uses no file:// credentials
// boots, and file:// URIs fail cleanly at resolve time instead of aborting the
// process at startup. A path whose parent is a regular file forces MkdirAll to
// fail with ENOTDIR — deterministic on every OS and privilege level (root
// included), unlike a read-only directory which root can bypass.
func TestNewDefaultCredentialResolver_FileStoreInitFailure_DoesNotAbort(t *testing.T) {
	t.Parallel()

	parent := filepath.Join(t.TempDir(), "iam-a-file")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed parent file: %v", err)
	}
	dir := filepath.Join(parent, "credentials") // parent is a file -> MkdirAll ENOTDIR

	res := newDefaultCredentialResolver(dir, discardLogger())
	if res == nil {
		t.Fatal("resolver must be built even when the file store cannot initialize (Finding 2)")
	}
	if _, err := res.Resolve(context.Background(), "file://x"); err == nil {
		t.Fatal("file:// must fail at resolve time, not abort the process at startup")
	}
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

// TestAwaitSupervisorShutdown_SkipsWaitWhenSupervisorAlreadyExited is the
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

// TestWaitForSupervisorRuntime_ReturnsPromptlyWhenSupervisorExitsEarly pins the
// early-exit path: when Run returns before publishing a runtime (an initial
// build/start failure — supStopped closed), the wait must report supEnded
// immediately instead of blocking the full ceiling. The fake clock is NEVER
// advanced, so a return at all proves the deadline was not waited out.
func TestWaitForSupervisorRuntime_ReturnsPromptlyWhenSupervisorExitsEarly(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(0, 0))

	supStopped := make(chan struct{})
	close(supStopped) // supervisor's Run already returned before a runtime appeared

	resCh := make(chan runtimeWaitResult, 1)
	go func() {
		resCh <- waitForSupervisorRuntime(
			func() *goruntime.Runtime { return nil }, // never publishes a runtime
			fake, initialRuntimeWait, supStopped,
		)
	}()

	select {
	case res := <-resCh:
		if res.runtime != nil {
			t.Fatal("no runtime should be produced when the supervisor exits early")
		}
		if !res.supEnded {
			t.Fatal("wait must report the supervisor ended before a runtime appeared")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait must return promptly on early supervisor exit, not block the ceiling")
	}
}

// TestWaitForSupervisorRuntime_RuntimeAppearingAfterSlowInitialBuildSucceeds
// pins the ceiling behaviour: a runtime that appears only after a slow
// SYNCHRONOUS initial build (simulated here by advancing the fake clock 29s —
// past the old 10s ceiling but well under initialRuntimeWait) is returned, not
// timed out. A ceiling at or below the build delay would have fired the deadline
// first and returned a false timeout.
func TestWaitForSupervisorRuntime_RuntimeAppearingAfterSlowInitialBuildSucceeds(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(0, 0))
	supStopped := make(chan struct{}) // Run keeps running; never closes

	var ready atomic.Bool
	sentinel := new(goruntime.Runtime)
	runtimeOf := func() *goruntime.Runtime {
		if ready.Load() {
			return sentinel
		}
		return nil
	}

	resCh := make(chan runtimeWaitResult, 1)
	go func() {
		resCh <- waitForSupervisorRuntime(runtimeOf, fake, initialRuntimeWait, supStopped)
	}()

	// Let the wait park in its select: the deadline timer plus one poll timer.
	waitForTimerCount(t, fake, 2)

	// The synchronous initial build (credential resolution + store construction)
	// takes ~29s — past the old 10s ceiling but well under initialRuntimeWait —
	// and only THEN does the supervisor publish its runtime.
	ready.Store(true)
	fake.Advance(29 * time.Second)

	select {
	case res := <-resCh:
		if res.runtime != sentinel {
			t.Fatalf("runtime appearing at 29s (< %s ceiling) must be returned, not timed out; got %+v",
				initialRuntimeWait, res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after the runtime became available")
	}
}

// waitForTimerCount blocks until the fake clock has at least n active timers,
// synchronising with a background goroutine that registers timers on startup
// (the documented clocktest pattern) before the test advances time. Paced by
// testutil/wait rather than a spin loop, which would compete for CPU with the
// goroutine whose registration it is waiting for.
func waitForTimerCount(t *testing.T, f *clocktest.Fake, n int) {
	t.Helper()
	wait.Until(t, 2*time.Second, "fake clock reaches the expected active timer count", func() bool {
		return f.TimerCount() >= n
	})
}

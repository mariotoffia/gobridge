package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// awaitSupervisorShutdown blocks — bounded by the shutdown deadline (done) —
// for the supervisor goroutine to report it has unwound after the root context
// was cancelled. When alreadyExited is true the supervisor already self-exited
// and its single result was consumed by the primary select in run(); there is
// nothing left to wait for, so it returns immediately rather than reading the
// now-drained supDone a second time. A second read would never complete and the
// call would block until the full ShutdownTimeout elapsed.
func awaitSupervisorShutdown(alreadyExited bool, supDone <-chan error, done <-chan struct{}, logger *slog.Logger) {
	if alreadyExited {
		return
	}
	select {
	case err := <-supDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("supervisor shutdown error", "error", err)
		}
	case <-done:
		logger.Error("supervisor shutdown timed out")
	}
}

// initialRuntimeWait bounds how long run() waits for the supervisor to publish
// its initial runtime before giving up and exiting non-zero. The runtime is
// published right after the SYNCHRONOUS initial build (Supervisor.buildRuntime)
// and a non-blocking Runtime.Start — broker/session connects run in background
// goroutines and never gate publication, so sup.Runtime() goes non-nil within
// milliseconds of a healthy build. This wait therefore exists to bound a slow or
// hung SYNCHRONOUS startup build (credential resolution, store construction),
// which is UNBOUNDED for the initial build — unlike reconfiguration swaps, whose
// build is bounded by defaultSwapDeadline. 60s tolerates a slow-but-healthy cold
// cloud start (e.g. SSM/STS credential resolve plus a DynamoDB describe or a
// SQLite migration) while still backstopping a hung dependency, and mirrors the
// swap build's defaultSwapDeadline. A genuine build error surfaces promptly via
// supStopped rather than waiting out the full ceiling.
const initialRuntimeWait = 60 * time.Second

// runtimeWaitResult reports the outcome of waitForSupervisorRuntime: either the
// initial runtime became available (runtime != nil), or the supervisor's Run
// returned before producing one (supEnded). Distinguishing the two lets run()
// surface the supervisor's real error instead of a misleading timeout, and exit
// promptly rather than blocking the full ceiling.
type runtimeWaitResult struct {
	runtime  *goruntime.Runtime
	supEnded bool
}

// waitForSupervisorRuntime blocks until runtimeOf returns a non-nil runtime, the
// supervisor's Run returns before publishing one (supStopped closed), or the
// ceiling elapses. It NEVER reads supDone: the single buffered result is left
// intact for the one downstream reader, so this wait cannot steal the shutdown
// read. supStopped is a close-only broadcast, safe to observe here and again
// downstream.
func waitForSupervisorRuntime(
	runtimeOf func() *goruntime.Runtime,
	clk clock.Clock,
	timeout time.Duration,
	supStopped <-chan struct{},
) runtimeWaitResult {
	// ESSENTIAL: runtime init poll
	deadline := clk.After(timeout)
	for {
		if rt := runtimeOf(); rt != nil {
			return runtimeWaitResult{runtime: rt}
		}
		select {
		case <-supStopped:
			// Run returned before publishing a runtime. Re-check once in case a
			// runtime raced in just as Run exited; otherwise report the early
			// exit so the caller surfaces the buffered supDone error instead of
			// waiting out the full ceiling.
			if rt := runtimeOf(); rt != nil {
				return runtimeWaitResult{runtime: rt}
			}
			return runtimeWaitResult{supEnded: true}
		case <-deadline:
			return runtimeWaitResult{}
		case <-clk.After(20 * time.Millisecond):
		}
	}
}

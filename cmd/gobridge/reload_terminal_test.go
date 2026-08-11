package main

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// waitForFakeTimers spins until the fake clock has at least n active timers, so
// a test can advance time only AFTER applyCommitted has entered its deadline
// wait. Bounded by a wall-clock deadline so a wiring regression fails fast.
func waitForFakeTimers(t *testing.T, f *clocktest.Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.TimerCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d fake timers (have %d)", n, f.TimerCount())
		}
		runtime.Gosched()
	}
}

// TestApplyCommitted_SlowSuccessfulSwapNotRolledBack is the CRITICAL regression:
// a swap that is slow-but-SUCCESSFUL must be reported as applied (nil), even if
// the caller's request context is cancelled while the swap is still in flight.
// The previous code bailed on request-ctx cancellation and returned an error, so
// the httpapi layer rolled the durable config back while the runtime adopted the
// "rejected" config — contradictory routing. After the fix, once the config is
// submitted the request ctx is ignored and applyCommitted resolves on the
// terminal onSwap result.
func TestApplyCommitted_SlowSuccessfulSwapNotRolledBack(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	fileCh := make(chan *ports.BridgeConfig)
	go p.run(runCtx, fileCh)

	cfg := testConfig("bridge-demo", 1, "info")
	reqCtx, cancelReq := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(reqCtx, cfg) }()

	// The Supervisor drains the merged channel: the config is now IN-FLIGHT.
	got := receiveConfig(t, p.changes())
	if got != cfg {
		t.Fatal("supervisor received a different config pointer than submitted")
	}

	// The caller's request context expires while the swap is still running.
	cancelReq()

	// The swap eventually SUCCEEDS. applyCommitted must report success, NOT a
	// rolled-back failure.
	p.onSwap(bridge.SwapEvent{NewConfig: got, Error: nil})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("slow-but-successful swap must report applied, got error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted did not return after the swap succeeded")
	}
}

// TestApplyCommitted_InFlightWhenNoTerminalWithinDeadline proves the DEFINITIVE
// terminal contract: when a submitted swap does not report a terminal result
// within the apply deadline, applyCommitted returns ErrApplyInFlight — the
// non-rollback-safe signal — rather than an ambiguous timeout the caller might
// roll back.
func TestApplyCommitted_InFlightWhenNoTerminalWithinDeadline(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(0, 0))
	p := newReloadPipeline(ports.NewRegistry(), discardLogger(),
		withReloadClock(fake), withApplyDeadline(90*time.Second))

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	fileCh := make(chan *ports.BridgeConfig)
	go p.run(runCtx, fileCh)

	cfg := testConfig("bridge-demo", 2, "info")
	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(context.Background(), cfg) }()

	// Submit (supervisor drains), then never report a terminal swap result.
	got := receiveConfig(t, p.changes())
	if got != cfg {
		t.Fatal("supervisor received a different config pointer than submitted")
	}

	// applyCommitted is now waiting on the apply-deadline timer; advance past it.
	waitForFakeTimers(t, fake, 1)
	fake.Advance(90 * time.Second)

	select {
	case err := <-errCh:
		if !errors.Is(err, ports.ErrApplyInFlight) {
			t.Fatalf("expected ErrApplyInFlight terminal signal, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted did not return after the apply deadline expired")
	}
}

// TestApplyCommitted_DefinitiveFailureIsRollbackSafe confirms a swap that
// definitively FAILS resolves to that error (not ErrApplyInFlight): the
// Supervisor recovered the old config, so the runtime is not on cfg and the
// caller may safely roll back.
func TestApplyCommitted_DefinitiveFailureIsRollbackSafe(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fileCh := make(chan *ports.BridgeConfig)
	go p.run(ctx, fileCh)

	cfg := testConfig("bridge-demo", 3, "info")
	swapErr := errors.New("build failed: bad transport")
	err := commit(t, ctx, p, cfg, swapErr)

	if err == nil {
		t.Fatal("a failed swap must return an error")
	}
	if errors.Is(err, ports.ErrApplyInFlight) {
		t.Fatalf("a definitive swap failure must NOT be reported as in-flight: %v", err)
	}
	if !errors.Is(err, swapErr) {
		t.Fatalf("the swap error must be wrapped, got: %v", err)
	}
}

// TestApplyCommitted_PreSubmissionCancelIsRollbackSafe confirms a request-ctx
// cancellation BEFORE the config reaches the Supervisor is a rollback-safe
// failure (the runtime is untouched): it wraps ctx.Err() and is NOT
// ErrApplyInFlight. run is not started, so p.admin has no reader and the
// submission never completes.
func TestApplyCommitted_PreSubmissionCancelIsRollbackSafe(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	reqCtx, cancelReq := context.WithCancel(context.Background())
	cfg := testConfig("bridge-demo", 4, "info")

	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(reqCtx, cfg) }()

	cancelReq()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-submission cancel must wrap ctx.Err(), got: %v", err)
		}
		if errors.Is(err, ports.ErrApplyInFlight) {
			t.Fatalf("pre-submission cancel is rollback-safe and must NOT be in-flight: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted did not return after pre-submission cancellation")
	}
}

// TestApplyCommitted_PausedDeferredIsInFlight proves onSwap is TOTAL: a paused
// bridge records the committed config without applying it and fires a DEFERRED
// swap event. applyCommitted must resolve immediately with the committed-not-
// applied signal (ports.ErrApplyInFlight, no rollback) instead of hanging the
// waiter until the apply deadline.
func TestApplyCommitted_PausedDeferredIsInFlight(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fileCh := make(chan *ports.BridgeConfig)
	go p.run(ctx, fileCh)

	cfg := testConfig("bridge-demo", 5, "info")
	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(context.Background(), cfg) }()

	got := receiveConfig(t, p.changes())
	if got != cfg {
		t.Fatal("supervisor received a different config pointer than submitted")
	}
	// Supervisor reports the swap as DEFERRED (bridge paused by admin).
	p.onSwap(bridge.SwapEvent{NewConfig: got, Deferred: true})

	select {
	case err := <-errCh:
		if !errors.Is(err, ports.ErrApplyInFlight) {
			t.Fatalf("a deferred (paused) apply must resolve to ErrApplyInFlight (no rollback), got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted hung on a deferred swap instead of resolving")
	}
}

// TestApplyCommitted_PipelineShutdownResolvesWaiter proves a config that was
// submitted to the Supervisor but never received a terminal onSwap before the
// pipeline stopped resolves to the committed-not-applied signal on shutdown,
// rather than blocking its waiter for the full apply deadline. On shutdown the
// durable config is already written, so a rollback would lose the operator's
// committed change — hence ports.ErrApplyInFlight (no rollback).
func TestApplyCommitted_PipelineShutdownResolvesWaiter(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	runCtx, cancelRun := context.WithCancel(context.Background())
	fileCh := make(chan *ports.BridgeConfig)
	go p.run(runCtx, fileCh)

	cfg := testConfig("bridge-demo", 6, "info")
	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(context.Background(), cfg) }()

	// Drain the forwarded config (waiter is now registered and pending) but do
	// NOT fire onSwap, then stop the pipeline.
	got := receiveConfig(t, p.changes())
	if got != cfg {
		t.Fatal("supervisor received a different config pointer than submitted")
	}
	cancelRun()

	select {
	case err := <-errCh:
		if !errors.Is(err, ports.ErrApplyInFlight) {
			t.Fatalf("a pending waiter on pipeline shutdown must resolve to ErrApplyInFlight (no rollback), got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted hung on pipeline shutdown instead of resolving")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Production contract: ErrApplyInFlight is POLL-TO-RESOLUTION.
//
// An in-band apply that outruns the apply deadline returns
// ports.ErrApplyInFlight — committed, not confirmed applied, never rolled back.
// The accepted tradeoff is that the applier does NOT push a completion
// notification to the (already-returned) caller. What it MUST provide is a
// pollable state that resolves on its own: while the swap is in flight the
// config manager reports desired-ahead-of-running (the divergence rendered on
// /deephealth as reconfigure_pending + desired_version/running_version), and it
// clears without operator action the moment the swap reports its terminal
// result.
//
// This is the executable form of that contract, driven against a REAL
// config.Manager so the pointer/content correlation behind the health
// projection is exercised rather than mocked.
// ═══════════════════════════════════════════════════════════════════════════

// TestApplyCommitted_InFlightResolvesByPolling_ProductionContract proves the
// full operator loop: commit → 202 committed_applying (ErrApplyInFlight) →
// poll: desired 2 / running 1, pending, NO apply error → the slow swap lands →
// poll: running 2, pending cleared.
func TestApplyCommitted_InFlightResolvesByPolling_ProductionContract(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Boot the manager on v1 and confirm it running: the baseline an operator
	// polls against.
	boot := testConfig("bridge-demo", 1, "info")
	watchCh := make(chan *ports.BridgeConfig, 1)
	mgr := config.NewManager(config.Layer{
		Name:    "file",
		Loader:  staticLoader{cfg: boot},
		Watcher: chanWatcher{ch: watchCh},
	})
	loaded, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("manager Load: %v", err)
	}
	mgr.NotifyApplyResult(loaded, nil)

	out, err := mgr.Watch(ctx)
	if err != nil {
		t.Fatalf("manager Watch: %v", err)
	}
	t.Cleanup(mgr.Stop)

	// The committed config reaches the manager and is emitted downstream; the
	// EXACT emitted pointer is what the applier carries, so the manager can
	// correlate the eventual result.
	watchCh <- testConfig("bridge-demo", 2, "info")
	desired := receiveConfig(t, out)

	fake := clocktest.NewAt(time.Unix(0, 0))
	p := newReloadPipeline(ports.NewRegistry(), discardLogger(),
		withReloadClock(fake), withApplyDeadline(60*time.Second),
		withApplyResultNotifier(mgr))
	fileCh := make(chan *ports.BridgeConfig)
	go p.run(ctx, fileCh)

	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(context.Background(), desired) }()

	submitted := receiveConfig(t, p.changes())
	if submitted != desired {
		t.Fatal("supervisor received a different config pointer than submitted")
	}

	// The swap is slow: no terminal result before the apply deadline.
	waitForFakeTimers(t, fake, 1)
	fake.Advance(60 * time.Second)

	select {
	case err := <-errCh:
		if !errors.Is(err, ports.ErrApplyInFlight) {
			t.Fatalf("a slow swap must resolve to ErrApplyInFlight (no rollback), got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted did not return after the apply deadline expired")
	}

	// ── Poll #1: the caller's ambiguity is resolvable from state ────────────
	if !mgr.ReconfigurePending() {
		t.Fatal("an in-flight apply must read as reconfigure_pending so a poll can distinguish it from done")
	}
	if v, ok := mgr.AppliedVersion(); !ok || v != 2 {
		t.Fatalf("desired_version must name the committed config, got %d (ok=%v)", v, ok)
	}
	if v, ok := mgr.RunningVersion(); !ok || v != 1 {
		t.Fatalf("running_version must still name the serving config while the swap is in flight, got %d (ok=%v)", v, ok)
	}
	if err := mgr.LastApplyError(); err != nil {
		t.Fatalf("an in-flight apply is NOT a failure; last_apply_error must stay empty, got: %v", err)
	}

	// ── The slow swap finally lands, with no operator action ────────────────
	p.onSwap(bridge.SwapEvent{NewConfig: submitted, Error: nil})

	// ── Poll #2: resolution is visible ──────────────────────────────────────
	if mgr.ReconfigurePending() {
		t.Fatal("a completed swap must clear reconfigure_pending; the poll would never resolve")
	}
	if v, ok := mgr.RunningVersion(); !ok || v != 2 {
		t.Fatalf("running_version must advance to the applied config, got %d (ok=%v)", v, ok)
	}
	if err := mgr.LastApplyError(); err != nil {
		t.Fatalf("successful resolution must leave no apply error, got: %v", err)
	}
}

// TestApplyCommitted_InFlightThenFailureIsPollable_ProductionContract is the
// other half of the contract: an in-flight apply that ultimately FAILS must
// resolve the poll to a definitive, actionable state (running stays on the old
// version, the error is named) rather than staying pending forever. Without
// this, "committed_applying" would be indistinguishable from "stuck".
func TestApplyCommitted_InFlightThenFailureIsPollable_ProductionContract(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	boot := testConfig("bridge-demo", 1, "info")
	watchCh := make(chan *ports.BridgeConfig, 1)
	mgr := config.NewManager(config.Layer{
		Name:    "file",
		Loader:  staticLoader{cfg: boot},
		Watcher: chanWatcher{ch: watchCh},
	})
	loaded, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("manager Load: %v", err)
	}
	mgr.NotifyApplyResult(loaded, nil)
	out, err := mgr.Watch(ctx)
	if err != nil {
		t.Fatalf("manager Watch: %v", err)
	}
	t.Cleanup(mgr.Stop)

	watchCh <- testConfig("bridge-demo", 2, "info")
	desired := receiveConfig(t, out)

	fake := clocktest.NewAt(time.Unix(0, 0))
	p := newReloadPipeline(ports.NewRegistry(), discardLogger(),
		withReloadClock(fake), withApplyDeadline(60*time.Second),
		withApplyResultNotifier(mgr))
	fileCh := make(chan *ports.BridgeConfig)
	go p.run(ctx, fileCh)

	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(context.Background(), desired) }()
	submitted := receiveConfig(t, p.changes())

	waitForFakeTimers(t, fake, 1)
	fake.Advance(60 * time.Second)
	if err := <-errCh; !errors.Is(err, ports.ErrApplyInFlight) {
		t.Fatalf("expected ErrApplyInFlight, got: %v", err)
	}

	swapErr := errors.New("build candidate runtime: broker unreachable")
	p.onSwap(bridge.SwapEvent{NewConfig: submitted, Error: swapErr})

	if !mgr.ReconfigurePending() {
		t.Fatal("a failed swap must keep the divergence visible: desired is not running")
	}
	if v, ok := mgr.RunningVersion(); !ok || v != 1 {
		t.Fatalf("a failed swap must leave running_version on the serving config, got %d (ok=%v)", v, ok)
	}
	if got := mgr.LastApplyError(); got == nil || !errors.Is(got, swapErr) {
		t.Fatalf("last_apply_error must name the definitive failure, got: %v", got)
	}
}

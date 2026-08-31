package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// boundedCallResult runs fn under a goroutine-raced HARD ceiling so the manager
// unblocks when the ceiling fires even if fn ignores ctx entirely. It mirrors
// the route dispatcher's boundedSend (runtime/route/dispatch.go:297): the two
// live in separate packages with no shared lower layer this workstream owns, so
// the shape is intentionally duplicated rather than extracted.
//
// A cooperative fn returns through the buffered channel and its result (its own
// ctx-cancel error included) is returned UNCHANGED, so this is transparent to
// well-behaved adapters. An fn that truly hangs ignoring ctx leaves its
// goroutine parked until it eventually returns; the buffered channel lets that
// goroutine complete and exit without a reader, so nothing else leaks — the
// parked goroutine is the deliberate, documented ceiling (Go cannot forcibly
// kill a goroutine).
//
// The ceiling is derived from the injected clock (never time.NewTimer) so it is
// deterministically drivable from tests via a fake clock and stays clear of the
// production timing audit. A non-positive ceiling means "no bound": the call is
// awaited to completion (used where the timing config disables the bound).
//
// A panic inside fn is captured and re-raised on the caller goroutine so panic
// semantics are preserved and a background-goroutine panic can never crash the
// process.
//
// It reports whether fn COMPLETED (returned, with or without its own error)
// within the ceiling. completed is false ONLY when the ceiling fired while fn was
// still parked (fn ignored ctx) — the caller then knows the operation did NOT
// actually finish and can refuse to proceed as if it had (a source Close that
// never returned has NOT stopped the subscription, so the lease must not be
// handed off;: a Reconcile that never returned is unrecoverable in-process and
// must escalate to terminal rather than restart-and-leak). A cooperative fn that
// returns its own error is completed==true: the operation ran to conclusion, it
// just failed.
func (m *Manager) boundedCallResult(ctx context.Context, ceiling time.Duration, what string, fn func(context.Context) error) (error, bool) {
	type callResult struct {
		err error
		rec any
	}
	done := make(chan callResult, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- callResult{rec: rec}
			}
		}()
		done <- callResult{err: fn(ctx)}
	}()

	settle := func(res callResult) (error, bool) {
		if res.rec != nil {
			panic(res.rec)
		}
		return res.err, true
	}

	if ceiling <= 0 {
		return settle(<-done)
	}

	timer := m.clk.NewTimer(ceiling)
	defer timer.Stop()
	select {
	case res := <-done:
		return settle(res)
	case <-timer.C():
		// Prefer a result that landed in the same tick as the ceiling so a call
		// that actually completed wins over the timeout.
		select {
		case res := <-done:
			return settle(res)
		default:
		}
		return shared.NewBridgeError(shared.ErrCodeTimeout, shared.ErrorTransient,
			fmt.Sprintf("%s did not complete within bound %v: %v", what, ceiling, ctx.Err())), false
	}
}

// boundedReconcile runs a reconnect-driven Reconcile behind the goroutine-raced
// hard ceiling. handleSessionEvent previously called Reconcile
// SYNCHRONOUSLY, so a broker SDK call that ignores ctx would block the renew
// select loop: the renewal timer case is never serviced, the local lease
// expires, and a standby seizes it while this session stays subscribed —
// split-brain plus renewal starvation. Racing the call against the same
// eventReconcileTimeout ceiling keeps the renew loop's timer serviceable
// regardless of a wedged adapter; on the ceiling the error propagates on the
// existing session-failure path (afterRenewLoopExit), which now CLOSES the
// source session before releasing the lease, so the wedged session
// is not left subscribed.
//
// The caller still passes an eventReconcileContext-bounded ctx: a COOPERATIVE
// adapter aborts through ctx (real-clock deadline) and its result is returned
// unchanged; the injected-clock ceiling here is the independent backstop for a
// ctx-ignoring adapter.
//
// bounded parked-Reconcile goroutines: unlike boundedSend (which has a
// pre-spawn in-flight latch capping parked senders at ≤1 per binding),
// boundedCallResult spawns a fresh goroutine on every reconnect and cannot
// forcibly kill a ctx-ignoring Reconcile. If a ceiling-fire merely restarted the
// session in place, a flapping broker would spawn one parked Reconcile goroutine
// per flap, unbounded. So a ceiling-fire (completed == false: the adapter ignored
// ctx and Reconcile is STILL parked) is treated as unrecoverable in-process — the
// SAME class as the wedged Close — and escalated to a terminal
// ErrSessionUnrecoverable. superviseSession flips the runtime terminal and the
// pod restart forcibly tears the wedged transport down at the OS level (socket
// close on process exit), capping parked Reconcile goroutines at ONE across the
// process lifetime. A COOPERATIVE Reconcile that merely FAILED (completed == true:
// it returned an error within the ceiling) is a genuine transient and keeps its
// isolated-restart semantics — its error is returned unchanged so the
// session-failure path restarts the one session, not the whole pod.
func (m *Manager) boundedReconcile(ctx context.Context, plan connectivity.SessionPlan) error {
	err, completed := m.boundedCallResult(ctx, m.eventReconcileTimeout(), "reconnect reconcile", func(c context.Context) error {
		return m.session.Reconcile(c, plan)
	})
	if err != nil && !completed {
		return fmt.Errorf("%w: reconnect reconcile ignored ctx and did not complete within the ceiling: %w",
			ErrSessionUnrecoverable, err)
	}
	if errors.Is(err, shared.ErrTransportClosedPermanently) {
		return fmt.Errorf("%w: source ingress could not be quiesced safely: %w", ErrSessionUnrecoverable, err)
	}
	return err
}

// closeSourceBounded closes the source session under the goroutine-raced hard
// ceiling so a wedged Close cannot hang the manager. It is used on the
// session-failure recovery path to STOP the old owner consuming
// BEFORE the lease is released and a standby can seize it. The context is
// detached (WithoutCancel) so the close still runs during shutdown, mirroring
// releaseOwnedLeaseBestEffort; the bound reuses releaseTimeout — the same
// bounded-teardown budget the lease Release uses.
//
// The DEADLINE on that context is load-bearing, not decoration. The ceiling
// below only decides how long the MANAGER waits; it cannot stop the adapter. A
// cooperative adapter needs a deadline of its own to abort on — without one, a
// slow but well-behaved disconnect keeps running past the ceiling, is
// classified as a wedge (completed == false), terminalizes the process, and
// extends the outage to the lease TTL. Passing the same bound as the ceiling
// means a cooperative Close always aborts and returns THROUGH the race, so only
// a genuinely ctx-ignoring adapter is ever judged wedged; the ceiling remains
// the backstop for exactly that adapter. The deadline is set closeAbortMargin
// EARLIER than the ceiling: both are armed within microseconds of each other,
// so without the margin an adapter that aborts exactly at its deadline still
// has to unwind faster than the ceiling timer — a coin flip whose losing side
// is a false wedge.
//
// It returns whether Close actually COMPLETED within the ceiling. A false return
// means the ceiling fired while Close was still parked (the adapter ignored ctx):
// the source is STILL subscribed, so the caller MUST NOT release the lease — a
// wedged-but-subscribed session cannot be safely handed off to a standby.
func (m *Manager) closeSourceBounded(ctx context.Context, reason string) bool {
	ceiling := m.releaseTimeout()
	abortAfter := ceiling - closeAbortMargin
	if abortAfter <= 0 {
		abortAfter = ceiling / 2
	}
	closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), abortAfter)
	defer cancelClose()
	err, completed := m.boundedCallResult(closeCtx, ceiling, "source session close", func(c context.Context) error {
		return m.session.Close(c)
	})
	if err != nil {
		m.log(ctx, slog.LevelWarn, "source session close during session-failure recovery failed or timed out",
			"reason", reason, "error", err, "completed", completed)
	}
	return completed
}

// closeAbortMargin is how much earlier than the manager's hard ceiling the
// source Close is told to give up, leaving it room to unwind and return through
// the race. It mirrors the route dispatcher's send-wedge margin: the cooperative
// abort must always beat the wedge verdict, or ordinary slowness is punished as
// a hang. A ceiling at or below the margin (a very short configured step-down
// grace) halves instead, keeping the same ordering at any scale.
const closeAbortMargin = 500 * time.Millisecond

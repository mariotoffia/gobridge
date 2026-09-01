package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// Stop gracefully shuts down the runtime. It cancels all goroutines,
// waits for them to finish, then closes sessions, stores, and telemetry.
// If ctx expires before goroutines finish, teardown still proceeds under a
// bounded detached context and an error is returned.
//
// Stop is idempotent and safe to call on a runtime that was BUILT but never
// Started: the composition-root supervisor calls Stop() on a runtime whose
// swap failed, so Stop must release every prep-opened resource — lease/outbox/
// DLQ stores, all opened sessions (including unmanaged binding sessions), and
// any session managers — even though no background goroutine ever ran. After
// Stop the runtime is single-use and cannot be restarted (ADR-0004).
func (rt *Runtime) Stop(ctx context.Context) (retErr error) {
	rt.mu.Lock()
	if (rt.terminal || rt.stopped) && !rt.running {
		// A prior Stop already transitioned this runtime to stopped/terminal.
		if rt.stopDone != nil {
			// That Stop's teardown may still be in flight. Block until it has
			// fully completed so a caller relying on "Stop returned ⇒ resources
			// released" does not proceed early — e.g. reopen a store whose handle
			// the first Stop is about to close.
			stopDone := rt.stopDone
			rt.mu.Unlock()
			select {
			case <-stopDone:
				// Report the teardown's ACTUAL outcome. Two callers race on every
				// SIGTERM (the Start-context watcher and the composition root's
				// own stop); returning nil to the loser let a root log a clean
				// stop over a drain that had in fact failed, and let a supervisor
				// treat a half-stopped runtime as cleanly swapped out.
				rt.mu.Lock()
				stopErr := rt.stopErr
				rt.mu.Unlock()
				return stopErr
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// No stopDone recorded: terminal was tripped without a Stop-driven
		// teardown ever starting. Idempotent no-op.
		rt.mu.Unlock()
		return nil
	}
	// This call performs the teardown (either the first Stop, or a Stop after a
	// background component tripped terminal but left running=true). Publish a
	// stopDone that any concurrent second Stop blocks on until we are done.
	stopDone := make(chan struct{})
	rt.stopDone = stopDone
	rt.running = false
	// A deliberate Stop is a clean pause, NOT an unrecoverable death: mark the
	// runtime stopped (single-use) but do NOT trip terminal. terminal keeps
	// whatever a background component already set (bridge.go component-failure
	// trips), so an ABNORMAL stop stays terminal and the liveness backstop still
	// restarts the process, while a clean admin/swap Stop leaves /live at 200.
	rt.stopped = true
	cancel := rt.cancel
	rt.mu.Unlock()

	// close(stopDone) MUST be the very last thing Stop does. Registered first
	// ⇒ runs last (LIFO), after closeCancel/flushCancel and after the return
	// value (errors.Join) is computed — so a blocked second Stop only observes
	// completion once every resource is released. The named return is published
	// as rt.stopErr in the same defer, BEFORE the close, so a second Stop that
	// wakes on stopDone always reads the finished value.
	defer func() {
		rt.mu.Lock()
		rt.stopErr = retErr
		rt.mu.Unlock()
		close(stopDone)
	}()

	if logging.DebugEnabled(rt.logger) {
		rt.logger.Log(ctx, logging.LevelDebug, "runtime stopping",
			"instance_id", rt.instanceID,
		)
	}

	// cancel is nil for a built-but-never-started runtime (Start assigns it).
	if cancel != nil {
		// Drain state machine (ports.RuntimeCommand.Stop). Only `running` was
		// flipped false above (`healthy` is intentionally left true — a clean
		// Stop is a pause, not a failure), so readiness (running && healthy) has
		// gone false. Readiness=false only sheds PUSH traffic: an upstream LB or
		// service mesh stops routing new requests to this instance. PULL
		// transports (SQS/Kafka receivers) are NOT gated by readiness and keep
		// pulling from the broker until `cancel()` below tears their loop down —
		// so there is a residual intake/redelivery window between readiness going
		// false and cancel firing. Now SETTLE already-accepted in-flight
		// deliveries BEFORE cancelling: aborting mid-send/mid-ack turns
		// every rolling restart into a duplicate/loss window (a canceled context
		// fails the source ack, so the broker redelivers an already-sent message),
		// whereas letting accepted work finish its send+settle first avoids it.
		//
		// This runs by DEFAULT, not only under WithStopQuiesce: a SIGTERM that
		// cancelled work before quiescing turned every rolling restart into a
		// duplicate window. The wait is bounded — deadline fallback:
		// when the budget or the caller ctx expires we cancel remaining work and
		// return, leaving any unsettled source to broker redelivery (at-least-once),
		// never silently acked. WaitQuiescent acquires rt.mu, which we released above.
		if budget := rt.stopDrainBudget(); budget > 0 && ctx.Err() == nil && rt.anyRouteInFlight() {
			qCtx, qCancel := context.WithTimeout(ctx, budget)
			if err := rt.WaitQuiescent(qCtx, QuiescenceOptions{}); err != nil && rt.logger != nil {
				rt.logger.Warn("stop drain did not fully settle in-flight deliveries before deadline; cancelling (unsettled sources rely on broker redelivery)",
					"instance_id", rt.instanceID, "budget", budget, "error", err)
			}
			qCancel()
		}
		cancel()
	}

	// Close credential refresher BEFORE session teardown so that a
	// rotation in flight cannot race ApplyCredentials against session
	// Close (see AttachCredentialCloser rationale). The closer is
	// invoked under a bounded timeout so a stuck watcher cannot hang
	// Stop past the user-supplied ctx.
	rt.mu.Lock()
	closeRefresher := rt.credRefresherClose
	rt.credRefresherClose = nil
	rt.mu.Unlock()

	closeTimeout := rt.shutdownTimeout
	if closeTimeout <= 0 {
		closeTimeout = 5 * time.Second
	}

	if closeRefresher != nil {
		refresherCtx, refresherCancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		// Spawn the closer with explicit lifetime; if it overruns the
		// bounded timer or the caller's ctx, we move on (best-effort)
		// rather than blocking Stop.
		refresherDone := make(chan struct{})
		go func() {
			defer close(refresherDone)
			closeRefresher(refresherCtx)
		}()
		select {
		case <-refresherDone:
		case <-refresherCtx.Done():
		case <-ctx.Done():
		}
		refresherCancel()
	}

	var errs []error

	// Wait-goroutine has explicit fire-and-forget lifetime: it survives
	// only until rt.wg drains. For a never-started runtime rt.wg is empty and
	// this returns immediately. If ctx fires first we record the error and
	// proceed to teardown under the bounded closeCtx (below).
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		rt.wg.Wait()
	}()

	drainersDone := false
	select {
	case <-waitDone:
		drainersDone = true
	case <-ctx.Done():
		// Pre-cancelled / early-expiring ctx: we stop waiting for background
		// goroutines and proceed to close sessions/telemetry below. A straggler
		// (e.g. a drainer still finalising) may therefore emit a counter or span
		// after its provider is closed. That is benign: OTel Counter/Start calls
		// on a shut-down provider are no-ops and the SDK is concurrency-safe, so
		// no panic, race, or corruption results (OTEL).
		//
		// Name the PHASE. Stop joins errors from four distinct phases, and a bare
		// "context deadline exceeded" left an operator unable to tell "routes and
		// drainers were still working" (raise drain_timeout, or shed ingress
		// earlier) from "a broker close hung" (a plugin fault) — different
		// remedies entirely.
		errs = append(errs, fmt.Errorf(
			"runtime: stop: background components (routes, drainers, session managers) "+
				"did not finish within the shutdown budget: %w", ctx.Err()))
	}

	// Manager-close gating. A drainer's finalDrain (outbox/loop.go)
	// runs under context.WithoutCancel plus a bounded grace, so it can still be
	// mid-send after the caller ctx expired above. Closing a session manager
	// (which releases the lease and closes the session) out from under such a
	// send makes the drainer's subsequent Complete fail with a stale fencing
	// token — the record resurfaces on restart as a duplicate send. So, exactly
	// as the store-close does below, wait a bounded grace for the drainer wg to
	// CONFIRM done BEFORE we close managers. The grace is
	// clampedStoreCloseGrace(ctx): the policy-derived worst-case, but never longer
	// than the incoming shutdown ctx's remaining deadline so the detached
	// (WithoutCancel) wait cannot outlive the platform kill budget and get
	// SIGKILLed mid-drain. If the grace elapses we still close managers (leases
	// MUST release so another instance can take over — a stale-token Complete from
	// a genuinely stuck drainer is the lesser evil), but in the common case
	// finalDrain's Complete runs against a live manager/lease.
	if !drainersDone {
		graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), rt.clampedStoreCloseGrace(ctx))
		select {
		case <-waitDone:
			drainersDone = true
		case <-graceCtx.Done():
		}
		graceCancel()
	}

	// Close/flush must complete regardless of caller ctx cancellation,
	// but we preserve values (trace/correlation) for logging.
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer closeCancel()

	// Snapshot the managers, sessions and stores under the lock, then RELEASE it
	// before invoking the (potentially blocking) plugin Close calls. A wedged
	// broker client's Close must not hold rt.mu — that would stall Role(),
	// DeepHealth and the /live+/ready probes for the whole Stop duration.
	rt.mu.Lock()
	managed := make(map[string]bool, len(rt.sessionMgrs))
	mgrs := make([]*session.Manager, 0, len(rt.sessionMgrs))
	for sid, mgr := range rt.sessionMgrs {
		managed[sid] = true
		mgrs = append(mgrs, mgr)
	}
	type sessRef struct {
		sid  string
		sess ports.Session
	}
	sessRefs := make([]sessRef, 0, len(rt.entries)+len(rt.sessionSenders))
	for _, entry := range rt.entries {
		sid := ""
		if entry.sessCfg != nil {
			sid = entry.sessCfg.SessionID
		}
		sessRefs = append(sessRefs, sessRef{sid: sid, sess: entry.session})
	}
	for sid, sse := range rt.sessionSenders {
		sessRefs = append(sessRefs, sessRef{sid: sid, sess: sse.session})
	}
	metrics := rt.metrics
	// Role-tagged so a close failure names the store an operator has to go and
	// look at, rather than an anonymous "close: file is locked".
	stores := []struct {
		role  string
		store any
	}{
		{"managed_subscriptions", rt.managedSubscriptionStore},
		{"outbox", rt.outboxStore},
		{"dlq", rt.dlqStore},
		{"lease", rt.leaseStore},
	}
	rt.mu.Unlock()

	for _, mgr := range mgrs {
		if err := mgr.Close(closeCtx); err != nil {
			errs = append(errs, fmt.Errorf("runtime: stop: closing session manager: %w", err))
		}
	}
	// close every session that no manager owns. A built-but-never-started
	// runtime has no managers at all, so ALL of its opened sessions land here;
	// a started runtime reaches here only for unmanaged binding sessions
	// (session senders never promoted to a drainer). Dedupe by pointer so a
	// session shared across entries/binding-senders is closed exactly once.
	closedSessions := make(map[ports.Session]bool)
	for _, ref := range sessRefs {
		if ref.sess == nil || managed[ref.sid] || closedSessions[ref.sess] {
			continue
		}
		closedSessions[ref.sess] = true
		if err := ref.sess.Close(closeCtx); err != nil {
			errs = append(errs, fmt.Errorf("runtime: stop: closing unmanaged session %q: %w", ref.sid, err))
		}
	}

	// The durable stores must NOT be released while a drainer's
	// finalDrain is still mid-send. finalDrain runs under context.WithoutCancel
	// plus a bounded grace (outbox/loop.go), so it can outlive the caller's Stop
	// ctx by design. Closing the SQLite handle underneath an in-flight Complete
	// would, after a SUCCESSFUL send, drop the terminal state update and
	// resurface the record on restart as a duplicate. Only release store handles
	// once the drainer wg has CONFIRMED done. The bounded grace-wait above
	// (before manager close) already gave the drainers up to storeCloseGrace to
	// confirm; if they still have not (a genuinely stuck drainer) we skip the
	// close and leak the handle rather than risk mid-send corruption — a fresh
	// runtime opens its own handles on reload, so a leaked handle is the strictly
	// safer failure.
	//
	// Closing the managers/sessions above can take time; a drainer whose grace
	// expired at the earlier wait may have finished during that window. Re-check
	// non-blockingly here so we release the handles instead of leaking them when
	// the drainer has, in fact, completed.
	if !drainersDone {
		select {
		case <-waitDone:
			drainersDone = true
		default:
		}
	}
	if drainersDone {
		// Release store resources (e.g. SQLite file handles). Stores that hold
		// OS resources implement io.Closer; in-memory stores do not and are
		// skipped. Reconfiguration always builds a fresh runtime with its own
		// store instances before Stopping the old one, so a closed handle is
		// never shared with a live runtime.
		for _, s := range stores {
			if c, ok := s.store.(io.Closer); ok {
				if err := c.Close(); err != nil {
					errs = append(errs, fmt.Errorf("runtime: stop: closing %s store: %w", s.role, err))
				}
			}
		}
	} else {
		errs = append(errs, errors.New("runtime: stop: drainers did not confirm done before store-close grace; leaving store handles open to avoid mid-send corruption"))
	}

	if metrics != nil {
		flushTimeout := rt.shutdownTimeout / 2
		if flushTimeout <= 0 {
			flushTimeout = 5 * time.Second
		}
		flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
		defer flushCancel()
		if err := metrics.Flush(flushCtx); err != nil {
			errs = append(errs, fmt.Errorf("runtime: stop: flushing metrics: %w", err))
		}
		// Stop FLUSHES buffered data on every runtime stop but does NOT Close the
		// exporter: the metrics exporter and tracer are shared by every runtime
		// across config reloads and are owned by the composition root that
		// supplied them, which Closes them exactly once at process shutdown. A
		// per-runtime Close here killed the shared CloudWatch flush goroutine on
		// the FIRST reload, so all later metrics were silently dropped for the
		// process lifetime.
		//
		// The tracer (ports.Tracer) exposes only Close, no Flush, so there is
		// nothing to flush per-stop; like the metrics exporter it is neither
		// flushed nor Closed here. Its owner — the composition root that passed it
		// via bridge.WithSupervisorTracer — Closes it exactly once at process
		// shutdown. (No in-tree composition root wires a real tracer today; the
		// cmd/gobridge example shows the owner-Closes-once pattern.)
	}

	return errors.Join(errs...)
}

// storeCloseGrace is the FLOOR that Stop waits for the drainer waitgroup to
// confirm done before (a) closing session managers and (b) releasing durable
// store handles, when the caller ctx has already expired. It is a
// floor, not the whole budget: effectiveStoreCloseGrace raises it to cover the
// configured policies' worst-case single in-flight completion so a legitimate
// final drainer send is never cut off mid-flight (see effectiveStoreCloseGrace).
const storeCloseGrace = 15 * time.Second

// completeBudgetCeiling mirrors the drainer's completeCtx/completeBudget clamp
// (outbox/retry.go): the post-send Complete/Release window is bounded to at most
// 5s (min(SendTimeout, 5s) for a positive SendTimeout). Kept here as a named
// ceiling so effectiveStoreCloseGrace derives the SAME worst-case the drainer
// actually uses without a ports/outbox change.
const completeBudgetCeiling = 5 * time.Second

// effectiveStoreCloseGrace returns the grace Stop must wait for the drainers to
// confirm done, coherent with the drainers' worst-case single in-flight send.
//
// The inherited hazard (outbox/retry.go:131-141): a drainer's finalDrain runs
// under context.WithoutCancel and can legitimately spend up to SendTimeout on the
// final send plus up to completeBudget() (== min(SendTimeout, 5s)) on the
// post-send Complete. If the bare 15s storeCloseGrace elapses first, Stop closes
// the session manager — which clears the lease — so the drainer's runtime-side
// post-send lease fence refuses the final Complete and the record resurfaces on
// restart as an AVOIDABLE duplicate. To stay coherent, the grace must be at least
// the largest such worst-case across every shared-outbox route policy:
//
//	worst(entry) = SendTimeout + min(SendTimeout, 5s)   // after WithDefaults
//	grace        = max(storeCloseGrace floor, max_entries worst(entry))
//
// The floor (15s) still applies when no policy demands more (e.g. no routes, or
// tiny SendTimeouts). Single-owner failover semantics are preserved: the grace is
// still a BOUNDED wait — once it elapses Stop closes managers and releases the
// lease regardless (the "lesser evil" of a stale-token Complete from a genuinely
// stuck drainer, per the wait sites' comments), so leases always eventually
// release for a standby to take over.
func (rt *Runtime) effectiveStoreCloseGrace() time.Duration {
	grace := storeCloseGrace
	rt.mu.Lock()
	entries := rt.entries
	rt.mu.Unlock()
	for i := range entries {
		p := entries[i].config.Policy.WithDefaults()
		st := p.SendTimeout
		if st <= 0 {
			continue
		}
		complete := st
		if complete > completeBudgetCeiling {
			complete = completeBudgetCeiling
		}
		if worst := st + complete; worst > grace {
			grace = worst
		}
	}
	return grace
}

// storeCloseGraceMargin is subtracted from the incoming shutdown ctx's remaining
// deadline when clamping the store-close grace, so the bounded manager-close wait
// leaves a little headroom for the caller to observe the clamp rather than
// consuming the ENTIRE remaining budget right up to the platform kill instant.
const storeCloseGraceMargin = 1 * time.Second

// clampedStoreCloseGrace bounds the derived store-close grace by the incoming
// shutdown ctx's remaining deadline.
//
// effectiveStoreCloseGrace can derive a grace as large as
// SendTimeout + min(SendTimeout, 5s) (~65s for a 60s SendTimeout), and the
// grace-wait DETACHES from ctx via context.WithoutCancel so the drain survives
// caller cancellation. Detaching an UNCLAMPED grace lets that wait outlive the
// platform's OWN kill budget — ECS StopTimeout / K8s terminationGracePeriod,
// default 60s — so the process is SIGKILLed mid-drain: the exact avoidable
// duplicate + lost in-flight the coherence raise was meant to PREVENT (raising
// the bare 15s floor is what created this exposure). So when the caller ctx
// carries a deadline, clamp the grace to the remaining time (minus a small margin
// for the close phase that follows); with no deadline the derived grace stands (a
// deadline-less Background caller cannot be SIGKILLed by a platform budget this
// layer can observe). The value-detachment (trace/correlation) at the wait site
// is unaffected — only the WAIT duration is bounded, never below zero.
func (rt *Runtime) clampedStoreCloseGrace(ctx context.Context) time.Duration {
	grace := rt.effectiveStoreCloseGrace()
	deadline, ok := ctx.Deadline()
	if !ok {
		return grace
	}
	// Compute the remaining budget via the injected clock (never time.Until):
	// rt.clk is clock.System (real wall-clock) in production — matching both the
	// caller's real-time ctx deadline and the WithTimeout timer below — and is a
	// fake clock only under test.
	if remaining := deadline.Sub(rt.clk.Now()) - storeCloseGraceMargin; remaining < grace {
		grace = remaining
	}
	if grace < 0 {
		grace = 0
	}
	return grace
}

// defaultStopDrainBudget caps the pre-cancel in-flight settle phase of Stop when
// no explicit WithStopQuiesce budget was set. It is the ceiling that keeps a Stop
// with a deadline-less caller ctx (e.g. context.Background()) from blocking
// forever behind a wedged sender: at worst Stop settles for this long, then falls
// through to cancel + broker redelivery (deadline fallback). A caller ctx with a
// shorter deadline still wins — the drain honours whichever fires first.
const defaultStopDrainBudget = 25 * time.Second

// stopDrainBudget returns the bounded budget for Stop's pre-cancel in-flight
// settle phase. An explicit WithStopQuiesce wins; otherwise the default ceiling
// applies. The caller ctx additionally bounds the wait (see the WithTimeout(ctx,
// budget) at the call site), so a short SIGTERM grace period is respected without
// this method having to inspect the deadline.
func (rt *Runtime) stopDrainBudget() time.Duration {
	if rt.stopQuiesce > 0 {
		return rt.stopQuiesce
	}
	return defaultStopDrainBudget
}

// anyRouteInFlight reports whether any route runner currently has an in-flight
// delivery. Stop uses it to SKIP the pre-cancel drain when the runtime is already
// quiescent — the common case — so an idle Stop cancels promptly instead of
// waiting out a settle/quiet window. A delivery accepted in the narrow window
// between this snapshot and cancel is the same at-least-once boundary the
// deadline fallback already documents (broker redelivery, never a silent ack).
func (rt *Runtime) anyRouteInFlight() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, e := range rt.entries {
		if e.runner != nil && e.runner.InFlight() > 0 {
			return true
		}
	}
	return false
}

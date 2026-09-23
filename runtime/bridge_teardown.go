package runtime

import (
	"context"
	"time"
)

// The budgets that bound a teardown, shared by Stop, which tears down the whole
// runtime, and Retire, which tears down the routes and sessions of one reload
// unit while the rest keeps running.

// storeCloseGrace is the FLOOR that a teardown waits for the drainers to
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

// effectiveStoreCloseGrace returns the grace a teardown must wait for the
// drainers of entries' routes to confirm done, coherent with the drainers'
// worst-case single in-flight send. Stop passes every route, Retire the routes
// of the unit it retires.
//
// The inherited hazard (outbox/retry.go:131-141): a drainer's finalDrain runs
// under context.WithoutCancel and can legitimately spend up to SendTimeout on the
// final send plus up to completeBudget() (== min(SendTimeout, 5s)) on the
// post-send Complete. If the bare 15s storeCloseGrace elapses first, the
// teardown closes the session manager — which clears the lease — so the
// drainer's runtime-side post-send lease fence refuses the final Complete and
// the record resurfaces on restart as an AVOIDABLE duplicate. To stay coherent,
// the grace must be at least the largest such worst-case across every
// shared-outbox route policy:
//
//	worst(entry) = SendTimeout + min(SendTimeout, 5s)   // after WithDefaults
//	grace        = max(storeCloseGrace floor, max_entries worst(entry))
//
// The floor (15s) still applies when no policy demands more (e.g. no routes, or
// tiny SendTimeouts). Single-owner failover semantics are preserved: the grace is
// still a BOUNDED wait — once it elapses the teardown closes managers and
// releases the lease regardless (the "lesser evil" of a stale-token Complete
// from a genuinely stuck drainer, per the wait sites' comments), so leases
// always eventually release for a standby to take over.
func effectiveStoreCloseGrace(entries []*routeEntry) time.Duration {
	grace := storeCloseGrace
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

// clampedStoreCloseGrace bounds the store-close grace derived from entries by
// the incoming shutdown ctx's remaining deadline.
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
func (rt *Runtime) clampedStoreCloseGrace(ctx context.Context, entries []*routeEntry) time.Duration {
	grace := effectiveStoreCloseGrace(entries)
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

// defaultStopDrainBudget caps the pre-cancel in-flight settle phase of a
// teardown when no explicit WithStopQuiesce budget was set. It is the ceiling
// that keeps a teardown with a deadline-less caller ctx (e.g.
// context.Background()) from blocking forever behind a wedged sender: at worst
// it settles for this long, then falls through to cancel + broker redelivery
// (deadline fallback). A caller ctx with a shorter deadline still wins — the
// drain honours whichever fires first.
const defaultStopDrainBudget = 25 * time.Second

// stopDrainBudget returns the bounded budget for a teardown's pre-cancel
// in-flight settle phase. An explicit WithStopQuiesce wins; otherwise the
// default ceiling applies. The caller ctx additionally bounds the wait (see the
// WithTimeout(ctx, budget) at the call sites), so a short SIGTERM grace period is
// respected without this method having to inspect the deadline.
func (rt *Runtime) stopDrainBudget() time.Duration {
	if rt.stopQuiesce > 0 {
		return rt.stopQuiesce
	}
	return defaultStopDrainBudget
}

// closeTimeout bounds closing the credential refreshers, session managers and
// sessions of a teardown: the configured shutdown timeout, or 5s when none is
// set.
func (rt *Runtime) closeTimeout() time.Duration {
	if rt.shutdownTimeout > 0 {
		return rt.shutdownTimeout
	}
	return 5 * time.Second
}

// anyInFlight reports whether any route runner of entries has an in-flight
// delivery.
func anyInFlight(entries []*routeEntry) bool {
	for _, e := range entries {
		if e.runner != nil && e.runner.InFlight() > 0 {
			return true
		}
	}
	return false
}

package route

import (
	"context"
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// sendHeld sends a held direct_hold delivery and retries a recoverable failure
// inside the bridge, with backoff, while the route's SendRetryBudget allows. The
// source message stays unsettled throughout, so the source is the buffer during
// a destination outage instead of the dead-letter store. It returns the last
// send's error; the caller then makes the same replay-or-dead-letter decision it
// always made. See ADR-0017.
//
// Each physical send gets its own SendTimeout context and runs through
// boundedSend unchanged, so every attempt keeps the per-send wedge ceiling. The
// OnDelivery callback and the OnAttempt hook fire once per physical send.
//
// The loop stops on success, on a non-recoverable error, when the delivery
// context is cancelled (the caller then abandons the delivery unsettled), when
// the route is wedged, and when the budget cannot cover the next wait. All of
// them are re-checked when a wait ENDS, so a state that changed while this
// delivery was parked — a wedge another delivery latched, a context the bridge
// cancelled, a wake that came back late — stops it BEFORE the next physical
// send rather than after it.
func (r *RouteRunner) sendHeld(ctx context.Context, sender ports.Sender, msg ports.OutboundMessage, plan routing.DispatchPlan, attempt int) error {
	start := r.clk.Now()
	for try := 1; ; try++ {
		err := r.sendOnce(ctx, sender, msg, plan, attempt)
		if !r.retryableInProcess(ctx, err) {
			return err
		}
		delay := RetryDelay(r.policy, try, err)
		if r.sendRetryBudgetSpent(start, delay) {
			return err
		}
		// Re-check the WHOLE retry predicate after the wait, not just the
		// delivery context the wait itself watches. A second delivery can wedge
		// the route while this one is parked, and a latched wedge is terminal:
		// the route is refusing new deliveries and escalating to the supervisor,
		// so this delivery must not start another physical send into it either.
		if !r.awaitSendRetry(ctx, delay) || !r.retryableInProcess(ctx, err) {
			return err
		}
		// Then re-measure the budget itself. A timer guarantees a MINIMUM delay
		// and nothing more: scheduler pressure or a GC pause can resume this
		// goroutine long after the budget the delay was measured against. The
		// bound has to hold on the wall clock, because both route-validator
		// rules size a held delivery as "the last send starts inside the budget
		// and runs at most one SendTimeout" — a send started after the budget
		// overruns the very source window those rules protect.
		if r.sendRetryBudgetSpent(start, 0) {
			return err
		}
		r.metrics.Counter(shared.MetricSendRetries, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
	}
}

// sendRetryBudgetSpent reports whether the route's in-process send-retry budget
// can still cover need — the delay a wait is about to take, or zero when only
// the time already spent is being re-measured after one. It counts
// SendRetryBudgetExhausted on the way out, so both places that give up on the
// budget report it identically.
//
// The comparison is against what is LEFT of the budget rather than elapsed plus
// need: a destination's RetryAfter hint is authoritative and used verbatim, so
// it can be near the largest duration there is, and adding even a second of
// elapsed time to that wraps the sum negative — which reads as "fits", arming a
// centuries-long timer on a one-minute route. The subtraction cannot overflow:
// the budget is positive here (retryableInProcess required it) and elapsed time
// is never negative.
func (r *RouteRunner) sendRetryBudgetSpent(start time.Time, need time.Duration) bool {
	if remaining := r.policy.SendRetryBudget - r.clk.Since(start); remaining > 0 && need <= remaining {
		return false
	}
	r.metrics.Counter(shared.MetricSendRetryBudgetExhausted, 1,
		shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
	return true
}

// retryableInProcess reports whether a failed send may be tried again inside
// the bridge. Cancellation is never retried: the bridge cancelling itself says
// nothing about the destination, and abandonIfCancelled must see it.
func (r *RouteRunner) retryableInProcess(ctx context.Context, err error) bool {
	return err != nil &&
		r.policy.SendRetryBudget > 0 &&
		shared.IsRecoverableError(err) &&
		ctx.Err() == nil && !errors.Is(err, context.Canceled) &&
		!r.isWedged()
}

// sendOnce is one physical send under its own SendTimeout.
func (r *RouteRunner) sendOnce(ctx context.Context, sender ports.Sender, msg ports.OutboundMessage, plan routing.DispatchPlan, attempt int) error {
	sendCtx, cancel := context.WithTimeout(ctx, r.policy.SendTimeout)
	defer cancel()
	err := r.boundedSend(sendCtx, sender, msg, plan.BindingID)
	r.invokeOnDelivery(msg.Envelope, err)
	r.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionEgress,
		RouteID:     r.routeID,
		BindingID:   plan.BindingID,
		Address:     plan.Address,
		Envelope:    msg.Envelope,
		Attempt:     attempt,
		MaxAttempts: r.policy.MaxReplayAttempts,
		Err:         err,
	})
	return err
}

// awaitSendRetry waits d on the injected clock. It reports false when the
// delivery context ends first — and also when the wait runs out on a context
// that is already done. The two can become ready in the same instant, and then
// the select arm is a coin flip; re-reading the context in the timer arm makes
// the outcome the same either way, so a delivery the bridge has given up on is
// never sent again by a sender that ignores its context.
func (r *RouteRunner) awaitSendRetry(ctx context.Context, d time.Duration) bool {
	t := r.clk.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

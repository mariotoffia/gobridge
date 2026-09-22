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
// the route is wedged, and when the next wait would end past the budget.
func (r *RouteRunner) sendHeld(ctx context.Context, sender ports.Sender, msg ports.OutboundMessage, plan routing.DispatchPlan, attempt int) error {
	start := r.clk.Now()
	for try := 1; ; try++ {
		err := r.sendOnce(ctx, sender, msg, plan, attempt)
		if !r.retryableInProcess(ctx, err) {
			return err
		}
		delay := RetryDelay(r.policy, try, err)
		if r.clk.Since(start)+delay > r.policy.SendRetryBudget {
			r.metrics.Counter(shared.MetricSendRetryBudgetExhausted, 1,
				shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
			return err
		}
		if !r.awaitSendRetry(ctx, delay) {
			return err
		}
		r.metrics.Counter(shared.MetricSendRetries, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
	}
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
// delivery context ends first.
func (r *RouteRunner) awaitSendRetry(ctx context.Context, d time.Duration) bool {
	t := r.clk.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-ctx.Done():
		return false
	}
}

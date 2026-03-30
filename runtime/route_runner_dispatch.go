package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// sendDirectHoldForBinding creates a dispatch plan for a specific binding ID
// and sends via the appropriate sender.
func (r *RouteRunner) sendDirectHoldForBinding(ctx context.Context, del ports.Delivery, env *domain.Envelope, bindingID string) error {
	for _, b := range r.bindings {
		if b.ID == bindingID {
			addr := b.Address
			if addr != "" {
				rendered, err := RenderAddress(addr, env.Headers)
				if err == nil {
					addr = rendered
				}
			}
			return r.sendDirectHold(ctx, del, env, domain.DispatchPlan{
				BindingID: b.ID,
				Address:   addr,
				Headers:   copyHeaders(b.Options),
			})
		}
	}
	return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("override binding %q not found", bindingID))
}

// hasBinding returns true if the route has a binding with the given ID.
func (r *RouteRunner) hasBinding(bindingID string) bool {
	for _, b := range r.bindings {
		if b.ID == bindingID {
			return true
		}
	}
	return false
}

func (r *RouteRunner) sendDirectHold(ctx context.Context, del ports.Delivery, env *domain.Envelope, plan domain.DispatchPlan) error {
	if plan.Address != "" {
		env.Subject = plan.Address
	}
	if plan.Headers != nil {
		env.Headers = domain.MergeHeaders(env.Headers, plan.Headers, true)
	}

	sender := r.senderForBinding(plan.BindingID)

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "direct hold send",
			"route", r.routeID,
			"binding_id", plan.BindingID,
			"address", plan.Address,
		)
	}

	sendCtx, sendCancel := context.WithTimeout(ctx, r.policy.SendTimeout)
	defer sendCancel()

	sendErr := sender.Send(sendCtx, env)
	if sendErr == nil {
		r.metrics.Counter(domain.MetricMessagesSent, 1,
			domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID})
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "direct hold ack",
				"route", r.routeID,
				"envelope_id", env.ID,
			)
		}
		return del.Ack(ctx)
	}

	if domain.IsRecoverableError(sendErr) {
		r.metrics.Counter(domain.MetricRouteErrors, 1,
			domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID})
		retryAfter := domain.GetRetryAfter(sendErr)
		return r.retryOrFallback(ctx, del, env, retryAfter, sendErr)
	}

	if dlqErr := r.dlq.Route(ctx, env, r.routeID, plan.BindingID, r.sessionIDForBinding(plan.BindingID), "", sendErr, 0); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
	}
	logging.Debug(r.logger, "routed to DLQ",
		"route", r.routeID,
		"envelope_id", env.ID,
		"binding_id", plan.BindingID,
		"error", sendErr,
	)
	r.emitDLQ("permanent")
	return del.Ack(ctx)
}

// senderForBinding returns the binding-specific sender if one is registered,
// otherwise falls back to the route's default sender.
func (r *RouteRunner) senderForBinding(bindingID string) ports.Sender {
	if r.senders != nil {
		if s, ok := r.senders[bindingID]; ok {
			return s
		}
	}
	return r.sender
}

func (r *RouteRunner) sessionIDForBinding(bindingID string) string {
	for _, b := range r.bindings {
		if b.ID == bindingID {
			return b.SessionID
		}
	}
	return ""
}

func (r *RouteRunner) handleExpired(ctx context.Context, del ports.Delivery, env *domain.Envelope) error {
	if r.policy.OnExpired == domain.ExpiredDLQ {
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", domain.ErrMessageExpired, 0); dlqErr != nil {
			return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
		}
		r.emitDLQ("expired")
	}
	return del.Ack(ctx)
}

func (r *RouteRunner) handleProcessorError(ctx context.Context, del ports.Delivery, env *domain.Envelope, err error) error {
	if errors.Is(err, domain.ErrMessageFiltered) {
		if r.policy.OnPermanentFailure == domain.FailureDLQ {
			if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", err, 0); dlqErr != nil {
				return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
			}
			r.emitDLQ("filtered")
		}
		return del.Ack(ctx)
	}
	if domain.IsRecoverableError(err) {
		r.metrics.Counter(domain.MetricRouteErrors, 1,
			domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID})
		retryAfter := domain.GetRetryAfter(err)
		return r.retryOrFallback(ctx, del, env, retryAfter, err)
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", err, 0); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
	}
	r.emitDLQ("permanent")
	return del.Ack(ctx)
}

func (r *RouteRunner) handleResolveError(ctx context.Context, del ports.Delivery, env *domain.Envelope, err error) error {
	be, ok := domain.AsBridgeError(err)
	if ok && be.Class != domain.ErrorTransient {
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", err, 0); dlqErr != nil {
			return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
		}
		r.emitDLQ("rejected")
		return del.Ack(ctx)
	}
	return r.retryOrFallback(ctx, del, env, 0, err)
}

// retryOrFallback attempts del.Retry; if the source transport does not
// support retry (ErrNotSupported), it falls back to DLQ routing with
// category "retry_unsupported" so the message is not silently lost.
func (r *RouteRunner) retryOrFallback(ctx context.Context, del ports.Delivery, env *domain.Envelope, after time.Duration, reason error) error {
	retryErr := del.Retry(ctx, after, reason)
	if retryErr == nil || !errors.Is(retryErr, domain.ErrNotSupported) {
		return retryErr
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", reason, 0); dlqErr != nil {
		r.emitDLQ("retry_unsupported_dlq_failed")
		return fmt.Errorf("retry unsupported and DLQ write failed: %w", dlqErr)
	}
	if !r.dlq.HasStore() {
		r.metrics.Counter(domain.MetricMessagesDropped, 1,
			domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID})
		if r.logger != nil {
			r.logger.Warn("message dropped: retry unsupported and no DLQ configured",
				"route", r.routeID, "envelope_id", env.ID)
		}
	}
	r.emitDLQ("retry_unsupported")
	return del.Ack(ctx)
}

func (r *RouteRunner) emitDLQ(category string) {
	r.metrics.Counter(domain.MetricDLQEntries, 1,
		domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID},
		domain.Tag{Key: domain.TagKeyCategory, Value: category},
	)
}

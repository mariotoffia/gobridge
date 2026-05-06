package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
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
				if err != nil {
					addrErr := domain.ErrInvalidTopic.
						WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
					return r.handleResolveError(ctx, del, env, addrErr)
				}
				addr = rendered
			}
			if strings.EqualFold(b.Transport, "mqtt") && addr != "" {
				if err := ValidateMQTTTopic(addr); err != nil {
					topicErr := domain.ErrInvalidTopic.
						WithMessage(fmt.Sprintf("binding %q: %v", b.ID, err))
					return r.handleResolveError(ctx, del, env, topicErr)
				}
			}
			return r.sendDirectHold(ctx, del, env, domain.DispatchPlan{
				BindingID: b.ID,
				Address:   addr,
				Headers:   copyHeaders(b.Headers),
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

	rc := receiveCount(env)
	attempt := rc + 1

	sendErr := sender.Send(sendCtx, env)

	r.invokeOnDelivery(env, sendErr)

	r.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionEgress,
		RouteID:     r.routeID,
		BindingID:   plan.BindingID,
		Envelope:    env,
		Attempt:     attempt,
		MaxAttempts: r.policy.MaxReplayAttempts,
		Err:         sendErr,
	})

	if sendErr == nil {
		r.metrics.Counter(domain.MetricMessagesSent, 1,
			domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID})
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "direct hold ack",
				"route", r.routeID,
				"envelope_id", env.ID,
			)
		}
		r.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     r.routeID,
			BindingID:   plan.BindingID,
			Envelope:    env,
			Attempt:     attempt,
			MaxAttempts: r.policy.MaxReplayAttempts,
			Terminal:    true,
		})
		return r.ackDelivery(ctx, del)
	}

	if domain.IsRecoverableError(sendErr) {
		r.metrics.Counter(domain.MetricRouteErrors, 1,
			domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID})

		if r.policy.MaxReplayAttempts > 0 && rc >= r.policy.MaxReplayAttempts {
			if logging.DebugEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelDebug, "max replay attempts exceeded in direct_hold",
					"route", r.routeID,
					"envelope_id", env.ID,
					"receive_count", rc,
					"max_replay_attempts", r.policy.MaxReplayAttempts,
				)
			}
			poisonErr := domain.NewBridgeError(domain.ErrCodePoisonMessage, domain.ErrorPermanent,
				fmt.Sprintf("direct_hold: receive count %d >= max replay attempts %d", rc, r.policy.MaxReplayAttempts))
			if dlqErr := r.dlq.Route(ctx, env, r.routeID, plan.BindingID,
				r.sessionIDForBinding(plan.BindingID), "", poisonErr, rc); dlqErr != nil {
				return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
			}
			r.emitDLQ("max_retries")
			r.hook.OnSettled(ctx, ports.DeliveryOutcome{
				Direction:   ports.DirectionEgress,
				RouteID:     r.routeID,
				BindingID:   plan.BindingID,
				Envelope:    env,
				Attempt:     attempt,
				MaxAttempts: r.policy.MaxReplayAttempts,
				Err:         poisonErr,
				Terminal:    true,
			})
			return r.ackDelivery(ctx, del)
		}

		return r.retryOrFallback(ctx, del, env, retryDelay(r.policy, receiveCount(env)+1, sendErr), sendErr)
	}

	if dlqErr := r.dlq.Route(ctx, env, r.routeID, plan.BindingID, r.sessionIDForBinding(plan.BindingID), "", sendErr, 0); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
	}
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "routed to DLQ",
			"route", r.routeID,
			"envelope_id", env.ID,
			"binding_id", plan.BindingID,
			"error", sendErr,
		)
	}
	r.emitDLQ("permanent")
	r.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     r.routeID,
		BindingID:   plan.BindingID,
		Envelope:    env,
		Attempt:     attempt,
		MaxAttempts: r.policy.MaxReplayAttempts,
		Err:         sendErr,
		Terminal:    true,
	})
	return r.ackDelivery(ctx, del)
}

// invokeOnDelivery calls the optional OnDelivery callback if configured,
// recovering from any panic so a misbehaving callback cannot kill the
// delivery goroutine.
func (r *RouteRunner) invokeOnDelivery(env *domain.Envelope, err error) {
	if r.onDelivery == nil {
		return
	}
	defer func() { _ = recover() }()
	r.onDelivery(env, err)
}

// invokeOnAck calls the optional OnAck callback if configured, recovering
// from any panic so a misbehaving callback cannot kill the delivery
// goroutine.
func (r *RouteRunner) invokeOnAck(env *domain.Envelope, err error) {
	if r.onAck == nil {
		return
	}
	defer func() { _ = recover() }()
	r.onAck(env, err)
}

// ackDelivery wraps del.Ack so OnAck observes the result.
func (r *RouteRunner) ackDelivery(ctx context.Context, del ports.Delivery) error {
	err := del.Ack(ctx)
	r.invokeOnAck(del.Envelope(), err)
	return err
}

// retryDelivery wraps del.Retry so OnAck observes the result.
func (r *RouteRunner) retryDelivery(ctx context.Context, del ports.Delivery, after time.Duration, reason error) error {
	err := del.Retry(ctx, after, reason)
	r.invokeOnAck(del.Envelope(), err)
	return err
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
	if r.logger != nil && bindingID != "" {
		r.logger.Warn("no binding found for dispatch plan",
			"route_id", r.routeID,
			"binding_id", bindingID,
		)
	}
	return ""
}

func (r *RouteRunner) handleExpired(ctx context.Context, del ports.Delivery, env *domain.Envelope) error {
	if r.policy.OnExpired == domain.ExpiredDLQ {
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", domain.ErrMessageExpired, 0); dlqErr != nil {
			return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
		}
		r.emitDLQ("expired")
	}
	return r.ackDelivery(ctx, del)
}

func (r *RouteRunner) handleProcessorError(ctx context.Context, del ports.Delivery, env *domain.Envelope, err error) error {
	if errors.Is(err, domain.ErrMessageFiltered) {
		if r.policy.OnPermanentFailure == domain.FailureDLQ {
			if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", err, 0); dlqErr != nil {
				return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
			}
			r.emitDLQ("filtered")
		}
		return r.ackDelivery(ctx, del)
	}
	if domain.IsRecoverableError(err) {
		r.metrics.Counter(domain.MetricRouteErrors, 1,
			domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID})
		return r.retryOrFallback(ctx, del, env, retryDelay(r.policy, receiveCount(env)+1, err), err)
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", err, 0); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
	}
	r.emitDLQ("permanent")
	return r.ackDelivery(ctx, del)
}

func (r *RouteRunner) handleResolveError(ctx context.Context, del ports.Delivery, env *domain.Envelope, err error) error {
	be, ok := domain.AsBridgeError(err)
	if ok && be.Class != domain.ErrorTransient {
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", err, 0); dlqErr != nil {
			return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
		}
		r.emitDLQ("rejected")
		return r.ackDelivery(ctx, del)
	}
	return r.retryOrFallback(ctx, del, env, 0, err)
}

// retryOrFallback attempts del.Retry; if the source transport does not
// support retry (ErrNotSupported), it falls back to DLQ routing with
// category "retry_unsupported" so the message is not silently lost.
func (r *RouteRunner) retryOrFallback(ctx context.Context, del ports.Delivery, env *domain.Envelope, after time.Duration, reason error) error {
	retryErr := r.retryDelivery(ctx, del, after, reason)
	if retryErr == nil || !errors.Is(retryErr, domain.ErrNotSupported) {
		return retryErr
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", reason, 0); dlqErr != nil {
		r.emitDLQ("retry_unsupported_dlq_failed")
		return fmt.Errorf("runtime: route-runner: retry unsupported and write dlq: %w", dlqErr)
	}
	if !r.dlq.HasStore() {
		r.metrics.Counter(domain.MetricMessagesDropped, 1,
			domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID})
		if r.logger != nil {
			r.logger.Warn("message dropped: retry unsupported and no DLQ configured",
				"route", r.routeID, "envelope_id", env.ID)
		}
		r.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     r.routeID,
			Envelope:    env,
			Attempt:     receiveCount(env) + 1,
			MaxAttempts: r.policy.MaxReplayAttempts,
			Err:         reason,
			Terminal:    true,
		})
	}
	r.emitDLQ("retry_unsupported")
	return r.ackDelivery(ctx, del)
}

// receiveCount extracts the transport-level receive count from envelope
// headers. SQS populates this as "sqs.ApproximateReceiveCount".
// Handles int, int64, float64, and string representations.
// Returns 0 when the header is absent or not convertible.
func receiveCount(env *domain.Envelope) int {
	if env.Headers == nil {
		return 0
	}
	v, ok := env.Headers["sqs.ApproximateReceiveCount"]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}

func (r *RouteRunner) emitDLQ(category string) {
	r.metrics.Counter(domain.MetricDLQEntries, 1,
		domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID},
		domain.Tag{Key: domain.TagKeyCategory, Value: category},
	)
}

func (r *RouteRunner) sharedOutbox(ctx context.Context, del ports.Delivery, env *domain.Envelope) error {
	var plans []domain.DispatchPlan

	// Consume HeaderRouteOverride set by processor chain.
	if override, ok := domain.GetHeaderString(env.Headers, domain.HeaderRouteOverride); ok {
		delete(env.Headers, domain.HeaderRouteOverride)
		if r.hasBinding(override) {
			for _, b := range r.bindings {
				if b.ID == override {
					addr := b.Address
					if addr != "" {
						rendered, err := RenderAddress(addr, env.Headers)
						if err != nil {
							addrErr := domain.ErrInvalidTopic.
								WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
							return r.handleResolveError(ctx, del, env, addrErr)
						}
						addr = rendered
					}
					if strings.EqualFold(b.Transport, "mqtt") && addr != "" {
						if err := ValidateMQTTTopic(addr); err != nil {
							topicErr := domain.ErrInvalidTopic.
								WithMessage(fmt.Sprintf("binding %q: %v", b.ID, err))
							return r.handleResolveError(ctx, del, env, topicErr)
						}
					}
					plans = []domain.DispatchPlan{{
						BindingID: b.ID, Address: addr, Headers: copyHeaders(b.Headers),
					}}
					break
				}
			}
		} else if r.logger != nil {
			r.logger.Warn("route override references unknown binding",
				"route", r.routeID, "override", override)
		}
	}

	if plans == nil {
		var err error
		plans, err = r.resolvePlans(ctx, env)
		if err != nil {
			return r.handleResolveError(ctx, del, env, err)
		}
	}

	if r.outboxStore == nil {
		return r.retryOrFallback(ctx, del, env, time.Second, fmt.Errorf("shared_outbox route %q: no OutboxStore configured", r.routeID))
	}

	// Depth check is advisory: concurrent goroutines may each see under-capacity
	// and collectively exceed MaxOutboxDepth. This is acceptable because the
	// outbox drainer will eventually process excess entries, and QueryPending
	// errors now fail the delivery (fail-closed) rather than silently bypassing.
	if r.policy.MaxOutboxDepth > 0 && r.depthCache != nil {
		partitionKey := r.outboxPartitionKey(plans)
		if partitionKey != "" && !r.depthCache.isUnderCapacity(partitionKey) {
			pending, qErr := r.outboxStore.QueryPending(ctx, partitionKey, r.policy.MaxOutboxDepth+1)
			if qErr != nil {
				return r.retryOrFallback(ctx, del, env, time.Second, fmt.Errorf("runtime: route-runner: query outbox depth: %w", qErr))
			}
			atCapacity := len(pending) >= r.policy.MaxOutboxDepth
			r.depthCache.update(partitionKey, atCapacity)
			if atCapacity {
				return r.retryOrFallback(ctx, del, env, 5*time.Second, fmt.Errorf("outbox at capacity (%d pending)", len(pending)))
			}
		}
	}

	records := r.buildOutboxRecords(env, plans)

	persistErr := r.outboxStore.Persist(ctx, records)
	if persistErr != nil {
		if errors.Is(persistErr, domain.ErrDuplicateRecord) {
			return r.ackDelivery(ctx, del)
		}
		return r.retryOrFallback(ctx, del, env, 0, persistErr)
	}

	return r.ackDelivery(ctx, del)
}

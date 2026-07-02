package route

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// sendDirectHoldForBinding creates a dispatch plan for a specific binding ID
// and sends via the appropriate sender.
func (r *RouteRunner) sendDirectHoldForBinding(ctx context.Context, del ports.Delivery, env *messaging.Envelope, bindingID string) error {
	for _, b := range r.bindings {
		if b.ID == bindingID {
			addr := b.Address
			if addr != "" {
				rendered, err := RenderAddress(addr, env.Headers())
				if err != nil {
					addrErr := shared.ErrInvalidTopic.
						WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
					return r.handleResolveError(ctx, del, env, addrErr)
				}
				addr = rendered
			}
			if addr != "" {
				if err := r.validateAddress(b.ID, addr); err != nil {
					return r.handleResolveError(ctx, del, env, err)
				}
			}
			return r.sendDirectHold(ctx, del, env, routing.DispatchPlan{
				BindingID: b.ID,
				Address:   addr,
				Headers:   CopyHeaders(b.Headers),
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

func (r *RouteRunner) sendDirectHold(ctx context.Context, del ports.Delivery, env *messaging.Envelope, plan routing.DispatchPlan) error {
	// Build an isolated outbound envelope so we never mutate the source
	// delivery envelope. The logical Subject is preserved; the destination
	// address travels via OutboundMessage.Address.
	outbound := env.Clone()
	// Drop the source transport's stale redelivery-count headers from the
	// outbound clone so they cannot ride this bridge-to-bridge hop and be
	// misread as the downstream bridge's own receiveCount (E5-FU1). The source
	// env is left intact: receiveCount(env) is re-read from it on retry/poison.
	stripInboundReceiveCounts(outbound)
	if plan.Headers != nil {
		outbound.StampHeaders(messaging.MergeHeaders(outbound.Headers(), plan.Headers, true))
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

	// Continue the distributed trace downstream: stamp the active span context
	// (this bridge hop) onto the outbound envelope's W3C headers so the
	// receiving service parents on this hop. Injecting into an empty carrier
	// keeps this allocation-light. SetHeader is the trusted per-key path —
	// ReplaceHeaders would strip the freshly stamped reserved headers. Done at
	// send time so processor header mutations are already applied.
	//
	// When a tracer stamped this hop the outbound W3C headers are made exactly
	// the propagator output: any stale upstream traceparent/tracestate the clone
	// carried (e.g. an invalid tracestate OTel dropped during Extract) is removed
	// first so it cannot ride alongside the fresh bridge span. When tracing is
	// disabled the tracer returns no keys, this block is skipped, and the
	// upstream headers pass through untouched (bridge-to-bridge trace continuity
	// preserved). The shared-outbox path is decoupled (drained after the ingress
	// span ends) and instead relies on the upstream traceparent preserved on the
	// persisted envelope (K1).
	if injected := r.tracer.Inject(sendCtx, map[string]any{}); len(injected) > 0 {
		outbound.DeleteHeader(messaging.HeaderTraceParent)
		outbound.DeleteHeader(messaging.HeaderTraceState)
		for k, v := range injected {
			outbound.SetHeader(k, v)
		}
	}

	sendErr := sender.Send(sendCtx, ports.OutboundMessage{Envelope: outbound, Address: plan.Address})

	r.invokeOnDelivery(outbound, sendErr)

	r.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionEgress,
		RouteID:     r.routeID,
		BindingID:   plan.BindingID,
		Address:     plan.Address,
		Envelope:    outbound,
		Attempt:     attempt,
		MaxAttempts: r.policy.MaxReplayAttempts,
		Err:         sendErr,
	})

	if sendErr == nil {
		r.metrics.Counter(shared.MetricMessagesSent, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "direct hold ack",
				"route", r.routeID,
				"envelope_id", env.ID(),
			)
		}
		r.hook.OnSettled(ctx, ports.DeliveryOutcome{
			Direction:   ports.DirectionEgress,
			RouteID:     r.routeID,
			BindingID:   plan.BindingID,
			Address:     plan.Address,
			Envelope:    outbound,
			Attempt:     attempt,
			MaxAttempts: r.policy.MaxReplayAttempts,
			Terminal:    true,
		})
		return r.ackDelivery(ctx, del)
	}

	if shared.IsRecoverableError(sendErr) {
		r.metrics.Counter(shared.MetricRouteErrors, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})

		if r.policy.MaxReplayAttempts > 0 && rc >= r.policy.MaxReplayAttempts {
			if logging.DebugEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelDebug, "max replay attempts exceeded in direct_hold",
					"route", r.routeID,
					"envelope_id", env.ID(),
					"receive_count", rc,
					"max_replay_attempts", r.policy.MaxReplayAttempts,
				)
			}
			poisonErr := shared.NewBridgeError(shared.ErrCodePoisonMessage, shared.ErrorPermanent,
				fmt.Sprintf("direct_hold: receive count %d >= max replay attempts %d", rc, r.policy.MaxReplayAttempts))
			if dlqErr := r.dlq.Route(ctx, outbound, r.routeID, plan.BindingID, plan.Address,
				r.sessionIDForBinding(plan.BindingID), "", poisonErr, rc); dlqErr != nil {
				return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
			}
			r.emitDLQ("max_retries")
			r.hook.OnSettled(ctx, ports.DeliveryOutcome{
				Direction:   ports.DirectionEgress,
				RouteID:     r.routeID,
				BindingID:   plan.BindingID,
				Address:     plan.Address,
				Envelope:    outbound,
				Attempt:     attempt,
				MaxAttempts: r.policy.MaxReplayAttempts,
				Err:         poisonErr,
				Terminal:    true,
			})
			return r.ackDelivery(ctx, del)
		}

		return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, sendErr), sendErr)
	}

	if dlqErr := r.dlq.Route(ctx, outbound, r.routeID, plan.BindingID, plan.Address, r.sessionIDForBinding(plan.BindingID), "", sendErr, 0); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
	}
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "routed to DLQ",
			"route", r.routeID,
			"envelope_id", env.ID(),
			"binding_id", plan.BindingID,
			"error", sendErr,
		)
	}
	r.emitDLQ("permanent")
	r.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionEgress,
		RouteID:     r.routeID,
		BindingID:   plan.BindingID,
		Address:     plan.Address,
		Envelope:    outbound,
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
func (r *RouteRunner) invokeOnDelivery(env *messaging.Envelope, err error) {
	if r.onDelivery == nil {
		return
	}
	defer func() { _ = recover() }()
	r.onDelivery(env, err)
}

// invokeOnAck calls the optional OnAck callback if configured, recovering
// from any panic so a misbehaving callback cannot kill the delivery
// goroutine.
func (r *RouteRunner) invokeOnAck(env *messaging.Envelope, err error) {
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

func (r *RouteRunner) handleExpired(ctx context.Context, del ports.Delivery, env *messaging.Envelope) error {
	if r.policy.OnExpired == routing.ExpiredDLQ {
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", shared.ErrMessageExpired, 0); dlqErr != nil {
			return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
		}
		r.emitDLQ("expired")
	}
	return r.ackDelivery(ctx, del)
}

func (r *RouteRunner) handleProcessorError(ctx context.Context, del ports.Delivery, env *messaging.Envelope, err error) error {
	if errors.Is(err, shared.ErrMessageFiltered) {
		if r.policy.OnPermanentFailure == routing.FailureDLQ {
			if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", err, 0); dlqErr != nil {
				return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
			}
			r.emitDLQ("filtered")
		}
		return r.ackDelivery(ctx, del)
	}
	if shared.IsRecoverableError(err) {
		r.metrics.Counter(shared.MetricRouteErrors, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
		return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, err), err)
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", err, 0); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
	}
	r.emitDLQ("permanent")
	return r.ackDelivery(ctx, del)
}

func (r *RouteRunner) handleResolveError(ctx context.Context, del ports.Delivery, env *messaging.Envelope, err error) error {
	be, ok := shared.AsBridgeError(err)
	if ok && be.Class != shared.ErrorTransient {
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", err, 0); dlqErr != nil {
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
func (r *RouteRunner) retryOrFallback(ctx context.Context, del ports.Delivery, env *messaging.Envelope, after time.Duration, reason error) error {
	retryErr := r.retryDelivery(ctx, del, after, reason)
	if retryErr == nil || !errors.Is(retryErr, shared.ErrNotSupported) {
		return retryErr
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", reason, 0); dlqErr != nil {
		r.emitDLQ("retry_unsupported_dlq_failed")
		return fmt.Errorf("runtime: route-runner: retry unsupported and write dlq: %w", dlqErr)
	}
	if !r.dlq.HasStore() {
		r.metrics.Counter(shared.MetricMessagesDropped, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
		if r.logger != nil {
			r.logger.Warn("message dropped: retry unsupported and no DLQ configured",
				"route", r.routeID, "envelope_id", env.ID())
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

// Transport redelivery-count headers. Each source transport stamps its own
// per-message delivery counter under a transport-scoped key, and each bases that
// counter differently. receiveCount normalizes them to a 1-based count
// (1 == first delivery) so MaxReplayAttempts behaves identically regardless of
// the source transport. The runtime cannot import the adapter packages (layer
// rule), so these keys mirror the adapter constants by value.
const (
	// headerSQSReceiveCount is 1-based: SQS ApproximateReceiveCount is 1 on the
	// first receive and increments on each redelivery.
	headerSQSReceiveCount = "sqs.ApproximateReceiveCount"
	// headerASBDeliveryCount is 1-based: the Azure Service Bus SDK normalizes the
	// raw AMQP delivery-count by +1 before exposing DeliveryCount (1 on first
	// delivery, azservicebus message.go), and the asb adapter stamps that value
	// verbatim.
	headerASBDeliveryCount = "asb.delivery-count"
	// headerAMQP10DeliveryCount is 0-based: the amqp10 adapter stamps the raw
	// AMQP 1.0 header delivery-count, defined as the number of PRIOR failed
	// delivery attempts (0 on first delivery). receiveCount adds 1 to align it
	// with the 1-based transports above.
	headerAMQP10DeliveryCount = "amqp10.delivery-count"
)

// receiveCount extracts the source transport's redelivery count from envelope
// headers, normalized to 1-based (1 == first delivery). Transports are checked
// in a fixed order; a delivery normally carries exactly one of these headers, so
// the order only matters in rare bridge-to-bridge topologies where a stale
// upstream header survives, in which case the first match wins. Returns 0 when
// no known counter header is present (treated as a first delivery for the
// retry cap).
func receiveCount(env *messaging.Envelope) int {
	h := env.Headers()
	if h == nil {
		return 0
	}
	if n, ok := headerInt(h, headerSQSReceiveCount); ok {
		return n
	}
	if n, ok := headerInt(h, headerASBDeliveryCount); ok {
		return n
	}
	if n, ok := headerInt(h, headerAMQP10DeliveryCount); ok {
		return n + 1 // 0-based AMQP delivery-count -> 1-based receive count
	}
	return 0
}

// stripInboundReceiveCounts removes every source-transport redelivery-count
// header from env. It is applied to the OUTBOUND (cloned) envelope at each
// egress chokepoint so a stale upstream count cannot ride a bridge-to-bridge
// hop and be misread as the downstream bridge's own receiveCount (E5-FU1).
// The downstream bridge re-establishes the count from its own transport's
// redelivery header (or treats the message as a first delivery). Never call
// this on a source envelope: receiveCount is re-read from the source on the
// retry/poison paths. DeleteHeader is nil-safe.
func stripInboundReceiveCounts(env *messaging.Envelope) {
	env.DeleteHeader(headerSQSReceiveCount)
	env.DeleteHeader(headerASBDeliveryCount)
	env.DeleteHeader(headerAMQP10DeliveryCount)
}

// headerInt reads key from h and coerces the common numeric encodings a
// transport adapter may stamp (int, int64, uint32, float64, decimal string)
// into an int. The second result is false when the key is absent or the value
// cannot be interpreted as an integer.
func headerInt(h map[string]any, key string) (int, bool) {
	v, ok := h[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case uint32:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i, true
		}
	}
	return 0, false
}

func (r *RouteRunner) emitDLQ(category string) {
	r.metrics.Counter(shared.MetricDLQEntries, 1,
		shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID},
		shared.Tag{Key: shared.TagKeyCategory, Value: category},
	)
}

func (r *RouteRunner) sharedOutbox(ctx context.Context, del ports.Delivery, env *messaging.Envelope) error {
	var plans []routing.DispatchPlan

	// Consume HeaderRouteOverride set by processor chain.
	if override, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderRouteOverride); ok {
		env.DeleteHeader(messaging.HeaderRouteOverride)
		if r.hasBinding(override) {
			for _, b := range r.bindings {
				if b.ID == override {
					addr := b.Address
					if addr != "" {
						rendered, err := RenderAddress(addr, env.Headers())
						if err != nil {
							addrErr := shared.ErrInvalidTopic.
								WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
							return r.handleResolveError(ctx, del, env, addrErr)
						}
						addr = rendered
					}
					if addr != "" {
						if err := r.validateAddress(b.ID, addr); err != nil {
							return r.handleResolveError(ctx, del, env, err)
						}
					}
					plans = []routing.DispatchPlan{{
						BindingID: b.ID, Address: addr, Headers: CopyHeaders(b.Headers),
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
		if partitionKey != "" && !r.depthCache.IsUnderCapacity(partitionKey) {
			pending, qErr := r.outboxStore.QueryPending(ctx, partitionKey, r.policy.MaxOutboxDepth+1)
			if qErr != nil {
				return r.retryOrFallback(ctx, del, env, time.Second, fmt.Errorf("runtime: route-runner: query outbox depth: %w", qErr))
			}
			atCapacity := len(pending) >= r.policy.MaxOutboxDepth
			r.depthCache.Update(partitionKey, atCapacity)
			if atCapacity {
				return r.retryOrFallback(ctx, del, env, 5*time.Second, fmt.Errorf("outbox at capacity (%d pending)", len(pending)))
			}
		}
	}

	records, buildErr := r.buildOutboxRecords(env, plans)
	if buildErr != nil {
		return r.retryOrFallback(ctx, del, env, 0, buildErr)
	}

	persistErr := r.outboxStore.Persist(ctx, records)
	if persistErr != nil {
		if errors.Is(persistErr, shared.ErrDuplicateRecord) {
			return r.ackDelivery(ctx, del)
		}
		return r.retryOrFallback(ctx, del, env, 0, persistErr)
	}

	return r.ackDelivery(ctx, del)
}

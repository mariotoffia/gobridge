package route

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	// E5-FU3: a redelivery-count header that is present but uninterpretable makes
	// receiveCount fail open to a first delivery (rc==0) so a good message is
	// never DLQ'd on a parse error — but that silently uncaps MaxReplayAttempts,
	// and the recoverable-retry path below would then retry a permanently-failing
	// send unbounded. Keep the fail-open value; surface the condition as a metric
	// (+ debug log) so the otherwise-silent unbounded retry is observable — it
	// re-emits on each redelivery of the same malformed message.
	if badKey := unparseableReceiveCountKey(env); badKey != "" {
		r.metrics.Counter(shared.MetricReceiveCountUnparseable, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "unparseable redelivery-count header; treating as first delivery",
				"route", r.routeID,
				"envelope_id", env.ID(),
				"header", badKey,
				"value", fmt.Sprintf("%v", env.Headers()[badKey]),
			)
		}
	}
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
	// preserved). The shared-outbox path is decoupled (drained later by a
	// separate drainer that no longer holds this span), so it stamps the same
	// active span onto the PERSISTED envelope at outbox-build time
	// (buildOutboxRecords) — symmetric with this hop — so a drained record still
	// propagates this bridge hop downstream rather than the bare upstream
	// traceparent the clone carried (OTEL-N3).
	if injected := r.tracer.Inject(sendCtx, map[string]any{}); len(injected) > 0 {
		outbound.DeleteHeader(messaging.HeaderTraceParent)
		outbound.DeleteHeader(messaging.HeaderTraceState)
		for k, v := range injected {
			outbound.SetHeader(k, v)
		}
	}

	sendErr := r.boundedSend(sendCtx, sender, ports.OutboundMessage{Envelope: outbound, Address: plan.Address})

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
			if r.dropOnPermanentFailure() {
				// on_permanent_failure=drop, or no DLQ store: dropping is the
				// only terminal option that honours the policy (retrying forever
				// is the very poison loop the cap prevents). Count it so the loss
				// is observable, never silent.
				r.emitDrop("max_retries")
			} else {
				if dlqErr := r.dlq.Route(ctx, outbound, r.routeID, plan.BindingID, plan.Address,
					r.sessionIDForBinding(plan.BindingID), "", poisonErr, rc); dlqErr != nil {
					return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, dlqErr), fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
				}
				r.emitDLQ("max_retries")
			}
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

	if r.dropOnPermanentFailure() {
		// on_permanent_failure=drop, or no DLQ store: drop-with-metric so the
		// failed message is not silently ACKed as if delivered AND the operator's
		// drop policy is honoured (payload not retained in the DLQ). Mirrors the
		// terminal decision the processor/resolve/poison paths route through.
		if r.logger != nil {
			r.logger.Warn("permanent send error dropped",
				"route", r.routeID, "envelope_id", env.ID(), "error", sendErr)
		}
		r.emitDrop("permanent")
	} else {
		if dlqErr := r.dlq.Route(ctx, outbound, r.routeID, plan.BindingID, plan.Address, r.sessionIDForBinding(plan.BindingID), "", sendErr, 0); dlqErr != nil {
			return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, dlqErr), fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
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
	}
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

// dropOnPermanentFailure is the single decision point every TERMINAL
// permanent/rejected path routes through: it reports whether a permanently
// failed / rejected delivery must be DROPPED (with metric) rather than written
// to the DLQ. It is true when the operator selected on_permanent_failure=drop
// (the payload must not be retained in a DLQ), or when no DLQ store is
// configured (a DLQEntries counter with no store behind it is a false signal
// and drop is the only terminal option). Centralising the decision keeps the
// direct_hold send-failure, resolve-error, processor-permanent and replay-cap
// poison paths consistent so the configured policy is honoured everywhere.
func (r *RouteRunner) dropOnPermanentFailure() bool {
	return r.policy.OnPermanentFailure == routing.FailureDrop || !r.dlq.HasStore()
}

// boundedSend runs sender.Send under ctx (already bounded by SendTimeout) but
// guarantees the DISPATCHER unblocks when ctx fires even if the sender ignores
// cancellation entirely. A cooperative sender returns via the buffered done
// channel and its real result (including its own ctx-cancel error) is returned
// unchanged, so this is transparent to well-behaved transports. A sender that
// truly hangs ignoring ctx leaves its goroutine parked until it eventually
// returns; the buffered channel lets that goroutine complete and exit without a
// reader, so nothing else leaks.
//
// A panic inside Send is captured and re-raised on the caller goroutine so the
// existing Run-loop panic-recovery / poison-gate semantics are preserved (a
// deterministically-panicking sender is still capped, not silently retried
// forever) and a background-goroutine panic can never crash the process.
//
// ponytail: the parked goroutine on a truly-hung sender is the deliberate
// ceiling — Go cannot forcibly kill a goroutine, and the dispatcher MUST unblock
// on shutdown/reconfig. The hard ceiling is a fresh SendTimeout timer that is
// INDEPENDENT of parent-ctx cancellation: ctx (already the SendTimeout-bounded
// sendCtx) is still passed to Send so a COOPERATIVE sender aborts promptly and
// reports through done even mid-shutdown, but a sender that ignores ctx entirely
// gets SendTimeout to return before we unblock. Deriving the ceiling from a
// timer rather than ctx.Done() is what lets an already-cancelled parent ctx
// still capture a fast sender's SUCCESS instead of mis-timing it out into a
// duplicate retry. On the ceiling we classify the send as a transient TIMEOUT so
// the delivery is RETRIED (the source redelivers) and NEVER falsely acked; a
// non-blocking read of done first lets a send that completed in the same tick as
// the ceiling win, closing the "sent-then-timeout" duplicate window as far as
// the cooperative race allows (the residual send-success-after-ceiling duplicate
// is inherent to any send timeout and is already a documented at-least-once
// window).
func (r *RouteRunner) boundedSend(ctx context.Context, sender ports.Sender, msg ports.OutboundMessage) error {
	type sendResult struct {
		err error
		rec any
	}
	done := make(chan sendResult, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- sendResult{rec: rec}
			}
		}()
		done <- sendResult{err: sender.Send(ctx, msg)}
	}()

	// Injected clock (never time.NewTimer): the production timing audit forbids a
	// real timer in this layer, and it keeps the ceiling deterministically
	// drivable from tests via a fake clock.
	ceiling := r.clk.NewTimer(r.policy.SendTimeout)
	defer ceiling.Stop()
	select {
	case res := <-done:
		if res.rec != nil {
			panic(res.rec)
		}
		return res.err
	case <-ceiling.C():
		// Prefer a result that landed in the same tick as the ceiling: a send
		// that actually completed must win over the timeout so we never retry an
		// already-delivered message (duplicate).
		select {
		case res := <-done:
			if res.rec != nil {
				panic(res.rec)
			}
			return res.err
		default:
		}
		if r.logger != nil {
			r.logger.Warn("sender did not return within send bound; unblocking dispatcher",
				"route", r.routeID,
				"send_timeout", r.policy.SendTimeout,
				"cause", ctx.Err(),
			)
		}
		return shared.NewBridgeError(shared.ErrCodeTimeout, shared.ErrorTransient,
			fmt.Sprintf("send did not complete within send bound %v: %v", r.policy.SendTimeout, ctx.Err()))
	}
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

// settleContext returns the context to use for a TERMINAL settle (the Ack after
// a successful send, or a DLQ-write→ack). When the delivery context is still
// live it is returned unchanged. When it has already been cancelled — the common
// shutdown case, where a send has JUST succeeded and honouring cancellation
// would abort the Ack and force an avoidable transport redelivery — its
// cancellation is stripped (values, incl. trace/correlation, preserved) and a
// short bound applied so a stuck transport cannot hang teardown. This mirrors
// the panic-recovery settle model in the Run loop (runner.go): a successful send
// always gets its Ack within the shutdown window. The returned CancelFunc is
// always non-nil and must be called by the caller.
func (r *RouteRunner) settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), r.panicRetryTimeout)
}

// ackDelivery wraps del.Ack so OnAck observes the result. The Ack is issued on a
// settleContext so a successful send always acks in the shutdown window instead
// of being aborted by a cancelled delivery ctx (which would trigger an avoidable
// transport redelivery / duplicate).
func (r *RouteRunner) ackDelivery(ctx context.Context, del ports.Delivery) error {
	settleCtx, cancel := r.settleContext(ctx)
	defer cancel()
	err := del.Ack(settleCtx)
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
	attempts := receiveCount(env) + 1
	// DLQ the expiry only when the policy asks for it AND a store exists.
	// Without a store, Router.Route is a silent no-op, so emitting a DLQ
	// counter there would be a false signal; drop-with-metric instead.
	if r.policy.OnExpired == routing.ExpiredDLQ && r.dlq.HasStore() {
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", shared.ErrMessageExpired, 0); dlqErr != nil {
			return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, dlqErr), fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
		}
		r.emitDLQ("expired")
		return r.settleTerminal(ctx, del, env, shared.ErrMessageExpired, attempts)
	}
	// Drop path (OnExpired=drop, or dlq requested with no store configured):
	// count the expiry-drop and settle terminally so the loss is never silent.
	r.emitExpired()
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "message expired; dropped",
			"route", r.routeID, "envelope_id", env.ID())
	}
	return r.settleTerminal(ctx, del, env, shared.ErrMessageExpired, attempts)
}

func (r *RouteRunner) handleProcessorError(ctx context.Context, del ports.Delivery, env *messaging.Envelope, err error) error {
	attempts := receiveCount(env) + 1
	if errors.Is(err, shared.ErrMessageFiltered) {
		// A processor deliberately dropped the message. Filter-drops have their
		// OWN policy (OnFiltered), defaulting to drop — decoupled from
		// OnPermanentFailure so an allow-list filter does not DLQ 100% of
		// non-matching traffic just because the permanent-failure sink is the
		// DLQ default. DLQ only when OnFiltered=dlq is explicitly set AND a store
		// exists; otherwise it is an intentional filter-drop that must still be
		// observable (metric + terminal hook).
		if r.policy.OnFiltered == routing.FilteredDLQ && r.dlq.HasStore() {
			if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", err, 0); dlqErr != nil {
				return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, dlqErr), fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
			}
			// Counted as DLQEntries{category=filtered}; do NOT also emit
			// MessagesFiltered — the conservation law counts each message once.
			r.emitDLQ("filtered")
			return r.settleTerminal(ctx, del, env, err, attempts)
		}
		r.emitFiltered(filteredProcessorName(err))
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "message filtered; dropped",
				"route", r.routeID, "envelope_id", env.ID())
		}
		return r.settleTerminal(ctx, del, env, err, attempts)
	}
	if shared.IsRecoverableError(err) {
		r.metrics.Counter(shared.MetricRouteErrors, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})

		// Replay-cap gate. A deterministically-transient chain failure — a
		// ProcessorTimeout (shared.ErrProcessorTimeout) from an oversized
		// payload, a catastrophic regex, or a hung transform — would otherwise
		// retry forever, each attempt holding a concurrency slot for the full
		// ProcessorTimeout and eventually wedging the route semaphore on brokers
		// without a native redrive cap. Mirror the sendDirectHold / recoverDelivery
		// gate: at or above MaxReplayAttempts, poison to the DLQ (or drop-with-
		// metric under OnPermanentFailure=drop / no store) and settle terminally.
		rc := receiveCount(env)
		if r.policy.MaxReplayAttempts > 0 && rc >= r.policy.MaxReplayAttempts {
			return r.poisonReplayCapExceeded(ctx, del, env, rc, err, "max_retries")
		}
		return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, rc+1, err), err)
	}
	// Permanent failure. Honour OnPermanentFailure=drop and the no-store case
	// by dropping-with-metric; otherwise write the DLQ record.
	if r.dropOnPermanentFailure() {
		r.emitDrop("permanent")
		if r.logger != nil {
			r.logger.Warn("permanent failure dropped",
				"route", r.routeID, "envelope_id", env.ID(), "error", err)
		}
		return r.settleTerminal(ctx, del, env, err, attempts)
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", err, 0); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, dlqErr), fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
	}
	r.emitDLQ("permanent")
	return r.settleTerminal(ctx, del, env, err, attempts)
}

// poisonReplayCapExceeded terminally sinks a delivery that has reached the
// MaxReplayAttempts cap on a deterministically-transient failure path (e.g. a
// repeating processor-chain timeout). It writes a poison record to the DLQ, or
// drops-with-metric under OnPermanentFailure=drop or when no DLQ store is
// configured, and settles the delivery exactly once. category tags both the
// poison message and the DLQ/drop metric. It returns a non-nil error only when
// the DLQ write itself failed, in which case the delivery is left unsettled and
// routed through retryOrFallback so the source redelivers rather than silently
// dropping a poison message that could not be persisted.
func (r *RouteRunner) poisonReplayCapExceeded(ctx context.Context, del ports.Delivery, env *messaging.Envelope, rc int, cause error, category string) error {
	attempts := rc + 1
	poisonErr := shared.NewBridgeError(shared.ErrCodePoisonMessage, shared.ErrorPermanent,
		fmt.Sprintf("%s: receive count %d >= max replay attempts %d: %v", category, rc, r.policy.MaxReplayAttempts, cause))

	if r.dropOnPermanentFailure() {
		r.emitDrop(category)
		if r.logger != nil {
			r.logger.Warn("replay cap reached; dropped",
				"route", r.routeID, "envelope_id", env.ID(),
				"receive_count", rc, "category", category, "error", cause)
		}
		return r.settleTerminal(ctx, del, env, poisonErr, attempts)
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", poisonErr, rc); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, dlqErr), fmt.Errorf("runtime: route-runner: write poison dlq: %w", dlqErr))
	}
	r.emitDLQ(category)
	return r.settleTerminal(ctx, del, env, poisonErr, attempts)
}

func (r *RouteRunner) handleResolveError(ctx context.Context, del ports.Delivery, env *messaging.Envelope, err error) error {
	be, ok := shared.AsBridgeError(err)
	if ok && be.Class != shared.ErrorTransient {
		attempts := receiveCount(env) + 1
		if r.dropOnPermanentFailure() {
			// on_permanent_failure=drop, or no DLQ store: a rejected/permanent
			// resolve error would otherwise be DLQ'd against the operator's drop
			// policy (or silently ACKed with no store). Drop-with-metric and
			// settle terminally instead.
			r.emitDrop("rejected")
			if r.logger != nil {
				r.logger.Warn("resolve error dropped",
					"route", r.routeID, "envelope_id", env.ID(), "error", err)
			}
			return r.settleTerminal(ctx, del, env, err, attempts)
		}
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", err, 0); dlqErr != nil {
			return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, receiveCount(env)+1, dlqErr), fmt.Errorf("runtime: route-runner: write dlq: %w", dlqErr))
		}
		r.emitDLQ("rejected")
		return r.settleTerminal(ctx, del, env, err, attempts)
	}
	// Transient (or unknown-so-recoverable) resolve error. Apply the same
	// replay-cap gate as handleProcessorError: a deterministically-failing
	// resolver (e.g. a persistently unreachable locator) would otherwise retry
	// forever, and previously re-dispatched with ZERO delay — an immediate hot
	// loop. At or above MaxReplayAttempts, poison terminally; below the cap,
	// retry with the policy's bounded backoff instead of zero.
	rc := receiveCount(env)
	if r.policy.MaxReplayAttempts > 0 && rc >= r.policy.MaxReplayAttempts {
		return r.poisonReplayCapExceeded(ctx, del, env, rc, err, "max_retries")
	}
	return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, rc+1, err), err)
}

// retryOrFallback attempts del.Retry; if the source transport does not
// support retry (ErrNotSupported), it falls back to DLQ routing with
// category "retry_unsupported" so the message is not silently lost.
func (r *RouteRunner) retryOrFallback(ctx context.Context, del ports.Delivery, env *messaging.Envelope, after time.Duration, reason error) error {
	retryErr := r.retryDelivery(ctx, del, after, reason)
	if retryErr == nil || !errors.Is(retryErr, shared.ErrNotSupported) {
		return retryErr
	}
	attempts := receiveCount(env) + 1
	if !r.dlq.HasStore() {
		// Source cannot retry and no DLQ store is configured: the message
		// would otherwise be silently ACKed. Drop-with-metric and settle
		// terminally (exactly one OnSettled). No emitDLQ here — a DLQEntries
		// counter with no store behind it is a false signal.
		r.emitDrop("retry_unsupported")
		if r.logger != nil {
			r.logger.Warn("message dropped: retry unsupported and no DLQ configured",
				"route", r.routeID, "envelope_id", env.ID())
		}
		return r.settleTerminal(ctx, del, env, reason, attempts)
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", reason, 0); dlqErr != nil {
		// Write failed: return the error so the caller does NOT ack; the
		// source redelivers. DLQWriteFailures is emitted inside the router.
		return fmt.Errorf("runtime: route-runner: retry unsupported and write dlq: %w", dlqErr)
	}
	r.emitDLQ("retry_unsupported")
	return r.settleTerminal(ctx, del, env, reason, attempts)
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

// receiveCountHeaderKeys lists the transport redelivery-count header keys in the
// precedence order receiveCount consults them. Declared once so the
// present-but-unparseable detector (unparseableReceiveCountKey) and the
// cross-module wire-key contract test iterate the exact same set as receiveCount
// without re-listing the constants. It is an immutable lookup shared by the
// detector and its pin test, not mutable state.
//
//nolint:gochecknoglobals // immutable shared wire-contract lookup, not state.
var receiveCountHeaderKeys = []string{
	headerSQSReceiveCount,
	headerASBDeliveryCount,
	headerAMQP10DeliveryCount,
}

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

// unparseableReceiveCountKey reports a transport redelivery-count header that is
// PRESENT on env but whose value headerInt cannot interpret as an integer,
// scanning in the same precedence order receiveCount uses. It returns a key ONLY
// when no equal-or-higher-precedence header supplied a usable count — i.e.
// exactly the case where receiveCount fails open to 0 (first delivery) and
// MaxReplayAttempts is silently uncapped (E5-FU3). A cleanly parsed count (even a
// literal 0) or a merely absent header yields "" (no signal), so a valid
// fallback count never produces a false positive. Callers use the non-empty key
// only to emit an observability signal; the fail-open value from receiveCount is
// left untouched.
func unparseableReceiveCountKey(env *messaging.Envelope) string {
	h := env.Headers()
	if h == nil {
		return ""
	}
	firstBad := ""
	for _, key := range receiveCountHeaderKeys {
		if _, present := h[key]; !present {
			continue
		}
		if _, ok := headerInt(h, key); ok {
			return "" // a usable count exists; receiveCount used it — no silent uncap
		}
		if firstBad == "" {
			firstBad = key
		}
	}
	return firstBad
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

// receiveCountAliases maps a source transport's configured identity — the
// RegisterTransportFactory name the operator wrote under `transport:`, or the
// adapter's canonical PluginConfig.Kind() discriminator — to the ONE
// redelivery-count header key that transport legitimately stamps. Only the three
// count-bearing transports appear; every other source (MQTT, AMQP 0-9-1, HTTP, …)
// stamps no native count header and is absent by design, so
// stripForeignReceiveCounts removes ALL THREE keys for it.
//
// The runtime cannot import the adapter packages (layer rule), so both the
// conventional short names and the canonical Kind() values are listed by value
// and must stay in sync with the adapters. Keys are matched case-insensitively.
//
//nolint:gochecknoglobals // immutable wire-contract lookup, not state.
var receiveCountAliases = map[string]string{
	"sqs":              headerSQSReceiveCount,
	"aws.sqs":          headerSQSReceiveCount,
	"asb":              headerASBDeliveryCount,
	"servicebus":       headerASBDeliveryCount,
	"azure.servicebus": headerASBDeliveryCount,
	"amqp10":           headerAMQP10DeliveryCount,
	"amqp1.0":          headerAMQP10DeliveryCount,
	"amqp.amqp10":      headerAMQP10DeliveryCount,
}

// nativeReceiveCountKey returns the redelivery-count header key the named source
// transport legitimately stamps and whether the transport is a known
// count-bearing one. The lookup is case-insensitive and trims surrounding space.
// A transport the runtime does not recognize (including a source registered under
// a wholly custom operator-chosen name) returns ("", false): the conservative
// choice, since trusting a foreign count is the ingress-forgery vulnerability
// (F3) this table closes.
func nativeReceiveCountKey(sourceTransport string) (string, bool) {
	key, ok := receiveCountAliases[strings.ToLower(strings.TrimSpace(sourceTransport))]
	return key, ok
}

// stripForeignReceiveCounts removes, on INGRESS, every transport
// redelivery-count header that the SOURCE transport does not itself stamp,
// closing the ingress redelivery-count forgery vector (F3): an untrusted producer
// on a count-less source (e.g. an MQTT device that copies arbitrary user
// properties straight into envelope headers) could otherwise inject
// sqs.ApproximateReceiveCount: 999 and have its FIRST delivery read as over the
// replay cap, poison-routing a healthy message to the DLQ without a single
// genuine retry (producer-controlled DLQ pollution + denial-of-retry).
//
// The receiving transport keeps ONLY its own native count key and strips the
// other two; a source with no native count key (MQTT, AMQP 0-9-1, HTTP, …) strips
// all three. It must run at the ingress reserved-header sanitization seam, BEFORE
// receiveCount is consulted, and pairs with StripReservedHeaders there (the count
// keys are transport-namespaced, NOT x-bridge.*-reserved, so that strip does not
// cover them).
//
// sourceTransport == "" disables stripping: a route constructed without a
// declared source transport (a unit test driving HandleDelivery directly, or any
// pre-plumbing caller) keeps the legacy behaviour so an explicitly-stamped count
// is still honoured. Production routes always carry a declared source transport
// via RouteConfig.SourceTransport, so the strip is active wherever it matters.
// DeleteHeader is nil-safe.
func stripForeignReceiveCounts(env *messaging.Envelope, sourceTransport string) {
	if sourceTransport == "" {
		return
	}
	own, _ := nativeReceiveCountKey(sourceTransport)
	for _, key := range receiveCountHeaderKeys {
		if key == own {
			continue
		}
		env.DeleteHeader(key)
	}
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

// emitDrop counts a terminal DROP: a message the runtime settled WITHOUT a DLQ
// record and without a successful send (permanent/rejected/retry-unsupported
// under a drop policy or a missing DLQ store). reason tags the cause so a
// single MessagesDropped series is splittable. Together with MessagesSent,
// MessagesFiltered, MessagesExpired and DLQEntries it closes the conservation
// law received = sent + dropped + filtered + expired + dlq + inflight.
func (r *RouteRunner) emitDrop(reason string) {
	r.metrics.Counter(shared.MetricMessagesDropped, 1,
		shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID},
		shared.Tag{Key: shared.TagKeyReason, Value: reason},
	)
}

// emitFiltered counts a message a processor deliberately discarded
// (ErrMessageFiltered) under OnFiltered=drop — a policy discard, kept distinct
// from a fault-driven drop. processor is the low-cardinality name of the
// filtering processor when it attributed the drop (empty otherwise); it is
// omitted from the tag set when empty to avoid an empty-value series.
func (r *RouteRunner) emitFiltered(processor string) {
	tags := make([]shared.Tag, 0, 2)
	tags = append(tags, shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
	if processor != "" {
		tags = append(tags, shared.Tag{Key: shared.TagKeyProcessor, Value: processor})
	}
	r.metrics.Counter(shared.MetricMessagesFiltered, 1, tags...)
}

// filteredProcessorName extracts the low-cardinality processor attribution a
// filter processor may attach to ErrMessageFiltered via
// .With(shared.TagKeyProcessor, name). Empty when the detail is absent, so a
// filtered error without attribution still emits (untagged) without panicking.
func filteredProcessorName(err error) string {
	be, ok := shared.AsBridgeError(err)
	if !ok {
		return ""
	}
	if v, ok := be.Context[shared.TagKeyProcessor].(string); ok {
		return v
	}
	return ""
}

// emitExpired counts a message dropped because it expired before delivery
// under OnExpired=drop.
func (r *RouteRunner) emitExpired() {
	r.metrics.Counter(shared.MetricMessagesExpired, 1,
		shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
}

// settleTerminal records exactly one terminal outcome for an ingress delivery:
// it fires a single OnSettled (Terminal=true) and ACKs the source. It is the
// convergence point for ingress terminal drop/DLQ paths so the "exactly once
// per terminal state" contract is enforced structurally rather than by
// repeating the OnSettled+ack pair at every call site.
func (r *RouteRunner) settleTerminal(ctx context.Context, del ports.Delivery, env *messaging.Envelope, cause error, attempts int) error {
	r.hook.OnSettled(ctx, ports.DeliveryOutcome{
		Direction:   ports.DirectionIngress,
		RouteID:     r.routeID,
		Envelope:    env,
		Attempt:     attempts,
		MaxAttempts: r.policy.MaxReplayAttempts,
		Err:         cause,
		Terminal:    true,
	})
	return r.ackDelivery(ctx, del)
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
	//
	// L3: a fan-out writes one record per plan, each into its own outbox
	// partition, so the check consults EVERY distinct target partition (not just
	// plans[0]'s). Backpressure applies if ANY leg's partition is at capacity — a
	// full partition must not accept another record regardless of which leg
	// targets it. Single-binding routes (the common case) iterate exactly once,
	// identical to the pre-L3 behavior. Per-partition logic is unchanged: a fresh
	// under-capacity cache entry skips the query; unknown/stale/at-capacity query.
	if r.policy.MaxOutboxDepth > 0 && r.depthCache != nil {
		for _, partitionKey := range r.distinctOutboxPartitions(plans) {
			if r.depthCache.IsUnderCapacity(partitionKey) {
				continue
			}
			pending, qErr := r.outboxStore.QueryPending(ctx, partitionKey, r.policy.MaxOutboxDepth+1)
			if qErr != nil {
				return r.retryOrFallback(ctx, del, env, time.Second, fmt.Errorf("runtime: route-runner: query outbox depth: %w", qErr))
			}
			atCapacity := len(pending) >= r.policy.MaxOutboxDepth
			r.depthCache.Update(partitionKey, atCapacity)
			if atCapacity {
				return r.retryOrFallback(ctx, del, env, 5*time.Second, fmt.Errorf("outbox partition %q at capacity (%d pending)", partitionKey, len(pending)))
			}
		}
	}

	records, buildErr := r.buildOutboxRecords(ctx, env, plans)
	if buildErr != nil {
		// Replay-cap gate (mirrors handleProcessorError). A permanently-failing
		// record build — a resolver emitting a BindingID absent from the route's
		// bindings, or the store rejecting an oversized record (helpers.go) — would
		// otherwise retry indefinitely. The gate reads the source's redelivery
		// count, so it only trips for COUNT-BEARING sources (SQS, ASB with a
		// delivery-count, AMQP 1.0): at or above the cap it poisons terminally.
		// Count-less sources (MQTT, AMQP 0-9-1) stamp no count, so receiveCount is
		// always 0 and the gate never fires — they get F1's bounded, paced retry
		// (never the old zero-delay hot loop) but no bridge-level cap, relying on
		// the broker's own redelivery limit. Below the cap: retry with the policy's
		// bounded backoff.
		rc := receiveCount(env)
		if r.policy.MaxReplayAttempts > 0 && rc >= r.policy.MaxReplayAttempts {
			return r.poisonReplayCapExceeded(ctx, del, env, rc, buildErr, "max_retries")
		}
		return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, rc+1, buildErr), buildErr)
	}

	persistErr := r.outboxStore.Persist(ctx, records)
	if persistErr != nil {
		if errors.Is(persistErr, shared.ErrDuplicateRecord) {
			return r.ackDelivery(ctx, del)
		}
		// Replay-cap gate (mirrors handleProcessorError). A permanently-failing
		// outbox persist would otherwise retry indefinitely. As above, the gate
		// reads the source redelivery count, so it only trips for count-bearing
		// sources (SQS, ASB, AMQP 1.0); count-less sources (MQTT, AMQP 0-9-1) get
		// F1's paced retry but no bridge-level cap. At or above the cap, poison
		// terminally; below it, retry with the policy's bounded backoff (never the
		// old zero-delay hot loop). The ErrDuplicateRecord branch above is a
		// success/ack path and is left untouched — a duplicate means the record is
		// already durable.
		rc := receiveCount(env)
		if r.policy.MaxReplayAttempts > 0 && rc >= r.policy.MaxReplayAttempts {
			return r.poisonReplayCapExceeded(ctx, del, env, rc, persistErr, "max_retries")
		}
		return r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, rc+1, persistErr), persistErr)
	}

	return r.ackDelivery(ctx, del)
}

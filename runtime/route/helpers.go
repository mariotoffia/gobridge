package route

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func (r *RouteRunner) outboxPartitionKey(plans []routing.DispatchPlan) string {
	if len(plans) == 0 {
		return ""
	}
	sessionID := r.sessionIDForBinding(plans[0].BindingID)
	return persistence.OutboxPartitionKey(sessionID, plans[0].BindingID)
}

func (r *RouteRunner) resolvePlans(ctx context.Context, env *messaging.Envelope) ([]routing.DispatchPlan, error) {
	plans, err := r.resolveRawPlans(ctx, env)
	if err != nil {
		return nil, err
	}
	if err := r.validatePlanAddresses(plans); err != nil {
		return nil, err
	}
	return plans, nil
}

// resolveRawPlans returns dispatch plans from the configured resolver
// (or from a single-binding fallback when none is configured) without
// performing transport-level address validation. Address validation is
// applied uniformly by resolvePlans so all plans, regardless of source,
// pass through the same per-binding AddressValidator dispatch.
func (r *RouteRunner) resolveRawPlans(ctx context.Context, env *messaging.Envelope) ([]routing.DispatchPlan, error) {
	if r.resolver != nil {
		return r.resolver.Resolve(ctx, env)
	}
	if len(r.bindings) > 0 {
		b := r.bindings[0]

		addr, err := RenderAddress(b.Address, env.Headers())
		if err != nil {
			return nil, shared.ErrInvalidTopic.
				WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
		}

		return []routing.DispatchPlan{{
			BindingID: b.ID,
			Address:   addr,
			Headers:   CopyHeaders(b.Headers),
		}}, nil
	}
	return []routing.DispatchPlan{{BindingID: r.routeID}}, nil
}

// validatePlanAddresses runs the per-binding AddressValidator (when
// registered) against every plan's rendered Address. This is the
// single point where the runtime applies transport-level address
// rules; AP-005 promoted this from a hardcoded MQTT branch in each
// dispatch site to a generic capability surfaced by TransportFactory.
// Bindings whose transport returns a nil validator are skipped.
func (r *RouteRunner) validatePlanAddresses(plans []routing.DispatchPlan) error {
	for _, p := range plans {
		if p.Address == "" {
			continue
		}
		if err := r.validateAddress(p.BindingID, p.Address); err != nil {
			return err
		}
	}
	return nil
}

// validateAddress invokes the AddressValidator registered for bindingID
// (if any). Returns a shared.ErrInvalidTopic-wrapped error on failure.
func (r *RouteRunner) validateAddress(bindingID, address string) error {
	v, ok := r.addressValidators[bindingID]
	if !ok || v == nil {
		return nil
	}
	if err := v.ValidateAddress(address); err != nil {
		return shared.ErrInvalidTopic.
			WithMessage(fmt.Sprintf("binding %q: %v", bindingID, err))
	}
	return nil
}

func (r *RouteRunner) buildOutboxRecords(ctx context.Context, env *messaging.Envelope, plans []routing.DispatchPlan) ([]*persistence.OutboxRecord, error) {
	now := r.clk.Now()
	records := make([]*persistence.OutboxRecord, len(plans))

	// Persist the envelope without the source transport's redelivery-count
	// headers so a drained record forwarded to the next hop does not carry a
	// stale count the downstream bridge would misread as its own (E5-FU1).
	// Clone so the source envelope (re-read by receiveCount on retry paths) is
	// untouched; NewOutboxRecord deep-clones again per record.
	persisted := env.Clone()
	stripInboundReceiveCounts(persisted)

	// Continue the distributed trace across the store-and-forward hop (OTEL-N3):
	// stamp THIS bridge's active ingress span onto the persisted envelope's W3C
	// headers so a record drained later — by a separate drainer that no longer
	// holds this span in context — still propagates a bridge hop downstream.
	// Without this the clone carries only the upstream traceparent, so the
	// receiving service parents on the upstream span (skipping this bridge) or,
	// absent one, starts a disconnected trace. Mirrors the direct-hold inject in
	// sendDirectHold: any stale upstream traceparent/tracestate is removed first
	// so it cannot ride alongside the fresh bridge span, SetHeader is the trusted
	// per-key path, and when tracing is disabled Inject returns no keys so the
	// block is skipped and the upstream headers pass through untouched. Applied
	// to the clone only; the source envelope is never mutated.
	if injected := r.tracer.Inject(ctx, map[string]any{}); len(injected) > 0 {
		persisted.DeleteHeader(messaging.HeaderTraceParent)
		persisted.DeleteHeader(messaging.HeaderTraceState)
		for k, v := range injected {
			persisted.SetHeader(k, v)
		}
	}

	for i, plan := range plans {
		sessionID := r.sessionIDForBinding(plan.BindingID)
		if sessionID == "" {
			// An empty session maps to a BINDING#<id> outbox partition that no
			// drainer ever polls — every drainer keys on SESSION#<sid>. Persisting
			// here would orphan the record while the source is ACKed: silent loss.
			// Fail closed; the caller routes this through retryOrFallback (retry or
			// DLQ), never a bare ACK. Reachable when a Resolver emits a BindingID
			// absent from the configured bindings, or a shared_outbox route resolves
			// a plan to a binding with no effective session.
			return nil, fmt.Errorf("runtime: route-runner: route %q: dispatch plan binding %q resolves to no session; outbox record would orphan under BINDING#%s and never drain", r.routeID, plan.BindingID, plan.BindingID)
		}
		rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
			ID:              generateID(),
			RouteID:         r.routeID,
			EnvelopeID:      env.ID(),
			BindingID:       plan.BindingID,
			SessionID:       sessionID,
			Address:         plan.Address,
			Envelope:        *persisted,
			DispatchHeaders: plan.Headers,
			CreatedAt:       now,
			ExpiresAt:       env.ExpiresAt(),
		})
		if err != nil {
			return nil, err
		}
		records[i] = rec
	}
	return records, nil
}

// mergeProcessedEnvelope copies the mutable, processor-owned state — subject,
// payload and headers (the documented processor extension points: SetSubject,
// SetPayload, and the header mutators) — from a quiescent chain clone back onto
// a destination envelope. It is called (a) per-frame in route/chain.go, merging
// a frame's private clone onto its caller's envelope once the processor
// goroutine has cleanly returned, and (b) once by RunChain's caller, merging the
// whole-chain clone onto the source after RunChain returns nil. Both callers
// invoke it only when the source goroutine has already returned, so src has no
// live concurrent writer. StampHeaders is the trusted whole-map setter (no
// reserved-prefix strip) so a processor-set reserved header such as
// HeaderRouteOverride survives the merge.
func mergeProcessedEnvelope(dst, src *messaging.Envelope) {
	dst.SetSubject(src.Subject())
	dst.SetPayload(src.Payload())
	dst.StampHeaders(src.HeadersSnapshot())
}

func (r *RouteRunner) injectHeaders(env *messaging.Envelope) {
	if env.Headers() == nil {
		env.ReplaceHeaders(make(map[string]any, 3))
	}
	if _, ok := env.Headers()[messaging.HeaderCorrelationID]; !ok {
		env.SetHeader(messaging.HeaderCorrelationID, generateID())
	}
	env.SetHeader(messaging.HeaderRouteID, r.routeID)
	env.SetHeader(messaging.HeaderSourceID, r.instanceID)
}

func (r *RouteRunner) acquireSlots(ctx context.Context) error {
	if r.sem != nil {
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.globalSem != nil {
		select {
		case r.globalSem <- struct{}{}:
		case <-ctx.Done():
			if r.sem != nil {
				<-r.sem
			}
			return ctx.Err()
		}
	}
	return nil
}

// releaseSlots releases in reverse acquisition order (global then per-route).
func (r *RouteRunner) releaseSlots() {
	if r.globalSem != nil {
		<-r.globalSem
	}
	if r.sem != nil {
		<-r.sem
	}
}

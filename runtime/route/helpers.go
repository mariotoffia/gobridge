package route

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// distinctOutboxPartitions returns the outbox partition keys a dispatch fan-out
// would write into — one per plan, deduplicated in first-seen order. A fan-out
// to N bindings that map to N distinct sessions yields N distinct partitions;
// bindings sharing a session collapse to one. The advisory depth check (L3)
// consults EVERY distinct partition, not just plans[0]'s, so a full partition on
// any fan-out leg still applies backpressure — a single-binding route (the
// common case) yields exactly one key, identical to the pre-behavior.
func (r *RouteRunner) distinctOutboxPartitions(plans []routing.DispatchPlan) []string {
	keys := make([]string, 0, len(plans))
	seen := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		sessionID := r.sessionIDForBinding(plan.BindingID)
		key := persistence.OutboxPartitionKey(sessionID, plan.BindingID)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

func (r *RouteRunner) resolvePlans(ctx context.Context, env *messaging.Envelope) ([]routing.DispatchPlan, error) {
	plans, err := r.resolveRawPlans(ctx, env)
	if err != nil {
		return nil, err
	}
	// fail CLOSED on a resolver that returns ZERO dispatch plans. BOTH
	// delivery modes resolve through this single choke point (direct_hold and
	// shared_outbox), so one guard closes the shared_outbox silent-ACK path: an
	// empty plan slice otherwise reached buildOutboxRecords, persisted zero
	// records, and ACKed the source — message loss with no DLQ/outbox evidence.
	// The error is a plain (non-BridgeError) value so handleResolveError treats
	// it as recoverable and routes it through the replay-cap gate — bounded
	// retry-with-backoff below MaxReplayAttempts, then poison to DLQ/drop at the
	// cap — exactly like the validatePlanBindings orphan guard below. A resolver
	// is a runtime function that may yield a valid plan on a later call, so
	// bounded retry-then-poison (not a silent success) is the correct treatment.
	if len(plans) == 0 {
		return nil, fmt.Errorf("runtime: route-runner: route %q: resolver returned no dispatch plans; refusing to settle with zero delivery (fail-closed)",
			r.routeID)
	}
	if err := r.validatePlanBindings(plans); err != nil {
		return nil, err
	}
	if err := r.validatePlanAddresses(plans); err != nil {
		return nil, err
	}
	return plans, nil
}

// validatePlanBindings fails CLOSED on any resolver plan that targets a binding
// NOT declared on a route that HAS declared bindings. A custom
// DestinationResolver returning a DispatchPlan whose BindingID is absent from
// r.bindings would otherwise reach senderForBinding, silently FALL BACK to the
// route DEFAULT sender, pass only generic address sanity (not the intended
// binding's validator), and ACK the source after a wrong-target send —
// delivering outside the configured binding set and bypassing that binding's
// sender/validator policy.
//
// The rejection is a RECOVERABLE error, matching the shared-outbox orphan guard
// (buildOutboxRecords): the caller routes it through handleResolveError, which
// applies the same replay-cap gate as every other transient failure — retry with
// the policy backoff below MaxReplayAttempts, then poison to DLQ/drop per policy
// at the cap. It is NEVER a default-sender send and NEVER a success ack. A
// resolver is a runtime function that may return a valid binding on a later
// call, so bounded retry-then-poison is the correct fail-closed treatment (the
// finding permits "unsettle" here) and keeps direct-hold consistent with the
// shared-outbox path, which already rejected these plans.
//
// Scope: the check applies ONLY when the route DECLARES bindings. A route with
// NO bindings is a default-sender-only route — there is no binding set to escape
// and no per-binding sender/validator to bypass, so every plan legitimately maps
// to the single default sender (resolver-driven addressing). Two BindingIDs are
// otherwise allowed even on a binding-declaring route:
//   - "" (empty) keeps its meaning of "route default sender".
//   - a declared binding id (hasBinding).
func (r *RouteRunner) validatePlanBindings(plans []routing.DispatchPlan) error {
	if len(r.bindings) == 0 {
		return nil
	}
	for _, p := range plans {
		if p.BindingID == "" {
			continue
		}
		if r.hasBinding(p.BindingID) {
			continue
		}
		return fmt.Errorf("runtime: route-runner: route %q: dispatch plan targets undeclared binding %q; refusing default-sender fallback (fail-closed)",
			r.routeID, p.BindingID)
	}
	return nil
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
// rules; promoted this from a hardcoded MQTT branch in each
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
//
// When a binding's transport registers NO AddressValidator, a minimal
// TRANSPORT-AGNOSTIC sanity check still runs: a {header} placeholder splices
// producer-controlled bytes straight into the destination address, so an
// otherwise-unvalidated rendered address is rejected when it is empty or carries
// an ASCII control character (NUL, CR, LF, TAB, other, or DEL) — bytes that
// are never legitimate in a transport address and are the classic
// address/header/log-injection vector. The check is deliberately conservative: it
// does NOT reject wildcard or other metacharacters (an MQTT '#'/'+', an AMQP
// '*'/'>'), whose legitimacy is transport-specific and belongs to the transport's
// own AddressValidator — which, when registered, supersedes this generic check
// entirely.
func (r *RouteRunner) validateAddress(bindingID, address string) error {
	v, ok := r.addressValidators[bindingID]
	if !ok || v == nil {
		if err := genericAddressSanity(address); err != nil {
			return shared.ErrInvalidTopic.
				WithMessage(fmt.Sprintf("binding %q: %v", bindingID, err))
		}
		return nil
	}
	if err := v.ValidateAddress(address); err != nil {
		return shared.ErrInvalidTopic.
			WithMessage(fmt.Sprintf("binding %q: %v", bindingID, err))
	}
	return nil
}

// genericAddressSanity is the transport-agnostic fallback address check applied
// only when a binding's transport registers no AddressValidator. It rejects
// an empty rendered address and any ASCII control character (the range below
// 0x20, or DEL 0x7f) — never valid in a transport destination and the vehicle for
// address/header/log injection when a {header} placeholder splices
// producer-controlled bytes into the address. It intentionally permits every
// printable character, including transport wildcards, whose legitimacy only a
// transport-specific validator can judge.
func genericAddressSanity(address string) error {
	if address == "" {
		return fmt.Errorf("rendered address is empty")
	}
	for i := 0; i < len(address); i++ {
		if c := address[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("rendered address contains control character 0x%02x at offset %d", c, i)
		}
	}
	return nil
}

func (r *RouteRunner) buildOutboxRecords(ctx context.Context, env *messaging.Envelope, plans []routing.DispatchPlan) ([]*persistence.OutboxRecord, error) {
	now := r.clk.Now()
	specs := make([]persistence.OutboxSpec, len(plans))

	// Persist the envelope without the source transport's redelivery-count
	// headers so a drained record forwarded to the next hop does not carry a
	// stale count the downstream bridge would misread as its own.
	// Clone so the source envelope (re-read by receiveCount on retry paths) is
	// untouched; NewOutboxRecords shares this finalized immutable snapshot
	// across fan-out records.
	persisted := env.Clone()
	stripInboundReceiveCounts(persisted)

	// Continue the distributed trace across the store-and-forward hop (OTEL):
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
		specs[i] = persistence.OutboxSpec{
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
		}
	}
	records, err := persistence.NewOutboxRecords(*persisted, specs)
	if err != nil {
		return nil, err
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
	dst.SharePayloadFrom(src)
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

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

func (r *RouteRunner) buildOutboxRecords(env *messaging.Envelope, plans []routing.DispatchPlan) ([]*persistence.OutboxRecord, error) {
	now := r.clk.Now()
	records := make([]*persistence.OutboxRecord, len(plans))

	for i, plan := range plans {
		sessionID := r.sessionIDForBinding(plan.BindingID)
		rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
			ID:              generateID(),
			RouteID:         r.routeID,
			EnvelopeID:      env.ID,
			BindingID:       plan.BindingID,
			SessionID:       sessionID,
			Address:         plan.Address,
			Envelope:        *env,
			DispatchHeaders: plan.Headers,
			CreatedAt:       now,
			ExpiresAt:       env.ExpiresAt,
		})
		if err != nil {
			return nil, err
		}
		records[i] = rec
	}
	return records, nil
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

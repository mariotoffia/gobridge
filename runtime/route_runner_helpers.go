package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain"
)

func (r *RouteRunner) outboxPartitionKey(plans []domain.DispatchPlan) string {
	if len(plans) == 0 {
		return ""
	}
	sessionID := r.sessionIDForBinding(plans[0].BindingID)
	return domain.OutboxPartitionKey(sessionID, plans[0].BindingID)
}

func (r *RouteRunner) resolvePlans(ctx context.Context, env *domain.Envelope) ([]domain.DispatchPlan, error) {
	if r.resolver != nil {
		return r.resolver.Resolve(ctx, env)
	}
	if len(r.bindings) > 0 {
		b := r.bindings[0]

		addr, err := RenderAddress(b.Address, env.Headers)
		if err != nil {
			return nil, domain.ErrInvalidTopic.
				WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
		}

		if strings.EqualFold(b.Transport, "mqtt") {
			if err := ValidateMQTTTopic(addr); err != nil {
				return nil, domain.ErrInvalidTopic.
					WithMessage(fmt.Sprintf("binding %q: %v", b.ID, err))
			}
		}

		return []domain.DispatchPlan{{
			BindingID: b.ID,
			Address:   addr,
			Headers:   copyHeaders(b.Options),
		}}, nil
	}
	return []domain.DispatchPlan{{BindingID: r.routeID}}, nil
}

func (r *RouteRunner) buildOutboxRecords(env *domain.Envelope, plans []domain.DispatchPlan) []domain.OutboxRecord {
	now := r.clk.Now()
	records := make([]domain.OutboxRecord, len(plans))

	for i, plan := range plans {
		sessionID := r.sessionIDForBinding(plan.BindingID)
		records[i] = domain.OutboxRecord{
			ID:              generateID(),
			RouteID:         r.routeID,
			EnvelopeID:      env.ID,
			BindingID:       plan.BindingID,
			SessionID:       sessionID,
			Address:         plan.Address,
			Envelope:        *env,
			DispatchHeaders: plan.Headers,
			Status:          domain.OutboxPending,
			CreatedAt:       now,
			ExpiresAt:       env.ExpiresAt,
		}
	}
	return records
}

func (r *RouteRunner) injectHeaders(env *domain.Envelope) {
	if env.Headers == nil {
		env.Headers = make(map[string]any, 3)
	}
	if _, ok := env.Headers[domain.HeaderCorrelationID]; !ok {
		env.Headers[domain.HeaderCorrelationID] = generateID()
	}
	env.Headers[domain.HeaderRouteID] = r.routeID
	env.Headers[domain.HeaderSourceID] = r.instanceID
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

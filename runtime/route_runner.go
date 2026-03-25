package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// RouteRunner executes the ingress pipeline for a single route.
// It supports both DirectHold and SharedOutbox delivery modes.
type RouteRunner struct {
	routeID     string
	policy      domain.RoutePolicy
	receiver    ports.Receiver
	sender      ports.Sender
	outboxStore ports.OutboxStore
	dlq         *DLQRouter
	resolver    ports.DestinationResolver
	processors  []ports.Processor
	bindings    []domain.DestinationBinding
	instanceID  string
	metrics     ports.MetricsExporter
	logger      *slog.Logger
	sem         chan struct{}
}

// RouteRunnerConfig holds the configuration for a RouteRunner.
type RouteRunnerConfig struct {
	RouteID     string
	Policy      domain.RoutePolicy
	Receiver    ports.Receiver
	Sender      ports.Sender
	OutboxStore ports.OutboxStore
	DLQ         *DLQRouter
	Resolver    ports.DestinationResolver
	Processors  []ports.Processor
	Bindings    []domain.DestinationBinding
	InstanceID  string
	Metrics     ports.MetricsExporter
	Logger      *slog.Logger
}

// NewRouteRunnerFromConfig creates a RouteRunner from a config struct.
func NewRouteRunnerFromConfig(cfg RouteRunnerConfig) *RouteRunner {
	return newRouteRunner(cfg)
}

func newRouteRunner(cfg RouteRunnerConfig) *RouteRunner {
	dlq := cfg.DLQ
	if dlq == nil {
		dlq = NewDLQRouter(nil)
	}
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	r := &RouteRunner{
		routeID:     cfg.RouteID,
		policy:      cfg.Policy.WithDefaults(),
		receiver:    cfg.Receiver,
		sender:      cfg.Sender,
		outboxStore: cfg.OutboxStore,
		dlq:         dlq,
		resolver:    cfg.Resolver,
		processors:  cfg.Processors,
		bindings:    cfg.Bindings,
		instanceID:  cfg.InstanceID,
		metrics:     m,
		logger:      cfg.Logger,
	}
	if r.policy.MaxInFlight > 0 {
		r.sem = make(chan struct{}, r.policy.MaxInFlight)
	}
	return r
}

// Run starts the receiver and processes deliveries until the context
// is cancelled or the receiver returns an unrecoverable error.
func (r *RouteRunner) Run(ctx context.Context) error {
	return r.receiver.Run(ctx, r.handleDelivery)
}

func (r *RouteRunner) handleDelivery(ctx context.Context, del ports.Delivery) error {
	if err := r.acquireSlot(ctx); err != nil {
		return err
	}
	defer r.releaseSlot()

	start := time.Now()

	env := del.Envelope()

	env.Headers = domain.StripReservedHeaders(env.Headers)
	r.injectHeaders(env)

	if env.IsExpired() {
		return r.handleExpired(ctx, del, env)
	}

	if err := RunChain(ctx, r.processors, env); err != nil {
		return r.handleProcessorError(ctx, del, env, err)
	}

	var deliveryErr error
	switch r.policy.DeliveryMode {
	case domain.DeliverySharedOutbox:
		deliveryErr = r.sharedOutbox(ctx, del, env)
	default:
		deliveryErr = r.directHold(ctx, del, env)
	}

	routeTag := domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID}
	r.metrics.Timer(domain.MetricDeliveryE2ELatency, time.Since(start), routeTag)

	return deliveryErr
}

func (r *RouteRunner) directHold(ctx context.Context, del ports.Delivery, env *domain.Envelope) error {
	plans, resolveErr := r.resolvePlans(ctx, env)
	if resolveErr != nil {
		return r.handleResolveError(ctx, del, env, resolveErr)
	}

	plan := plans[0]
	if plan.Address != "" {
		env.Subject = plan.Address
	}
	if plan.Headers != nil {
		env.Headers = domain.MergeHeaders(env.Headers, plan.Headers, true)
	}

	sendErr := r.sender.Send(ctx, env)
	if sendErr == nil {
		return del.Ack(ctx)
	}

	if domain.IsRecoverableError(sendErr) {
		retryAfter := domain.GetRetryAfter(sendErr)
		return del.Retry(ctx, retryAfter, sendErr)
	}

	if dlqErr := r.dlq.Route(ctx, env, r.routeID, plan.BindingID, r.sessionIDForBinding(plan.BindingID), "", sendErr, 0); dlqErr != nil {
		return del.Retry(ctx, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
	}
	r.emitDLQ("permanent")
	return del.Ack(ctx)
}

func (r *RouteRunner) sharedOutbox(ctx context.Context, del ports.Delivery, env *domain.Envelope) error {
	plans, err := r.resolvePlans(ctx, env)
	if err != nil {
		return r.handleResolveError(ctx, del, env, err)
	}

	if r.policy.MaxOutboxDepth > 0 && r.outboxStore != nil {
		partitionKey := r.outboxPartitionKey(plans)
		if partitionKey != "" {
			pending, qErr := r.outboxStore.QueryPending(ctx, partitionKey, r.policy.MaxOutboxDepth+1)
			if qErr == nil && len(pending) >= r.policy.MaxOutboxDepth {
				return del.Retry(ctx, 5*time.Second, fmt.Errorf("outbox at capacity (%d pending)", len(pending)))
			}
		}
	}

	records := r.buildOutboxRecords(env, plans)

	persistErr := r.outboxStore.Persist(ctx, records)
	if persistErr != nil {
		if errors.Is(persistErr, domain.ErrDuplicateRecord) {
			return del.Ack(ctx)
		}
		return del.Retry(ctx, 0, persistErr)
	}

	return del.Ack(ctx)
}

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
		return []domain.DispatchPlan{{
			BindingID: b.ID,
			Address:   b.Address,
		}}, nil
	}
	return []domain.DispatchPlan{{BindingID: r.routeID}}, nil
}

func (r *RouteRunner) buildOutboxRecords(env *domain.Envelope, plans []domain.DispatchPlan) []domain.OutboxRecord {
	now := time.Now()
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
			return del.Retry(ctx, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
		}
		r.emitDLQ("expired")
	}
	return del.Ack(ctx)
}

func (r *RouteRunner) handleProcessorError(ctx context.Context, del ports.Delivery, env *domain.Envelope, err error) error {
	if domain.IsRecoverableError(err) {
		retryAfter := domain.GetRetryAfter(err)
		return del.Retry(ctx, retryAfter, err)
	}
	if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", err, 0); dlqErr != nil {
		return del.Retry(ctx, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
	}
	r.emitDLQ("permanent")
	return del.Ack(ctx)
}

func (r *RouteRunner) handleResolveError(ctx context.Context, del ports.Delivery, env *domain.Envelope, err error) error {
	be, ok := domain.AsBridgeError(err)
	if ok && be.Class != domain.ErrorTransient {
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", err, 0); dlqErr != nil {
			return del.Retry(ctx, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
		}
		r.emitDLQ("rejected")
		return del.Ack(ctx)
	}
	return del.Retry(ctx, 0, err)
}

func (r *RouteRunner) emitDLQ(category string) {
	r.metrics.Counter(domain.MetricDLQEntries, 1,
		domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID},
		domain.Tag{Key: domain.TagKeyCategory, Value: category},
	)
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

func (r *RouteRunner) acquireSlot(ctx context.Context) error {
	if r.sem == nil {
		return nil
	}
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *RouteRunner) releaseSlot() {
	if r.sem != nil {
		<-r.sem
	}
}

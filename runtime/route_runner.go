package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	goruntime "runtime/debug"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/observability"
	"github.com/mariotoffia/gobridge/ports"
)

// RouteRunner executes the ingress pipeline for a single route.
// It supports both DirectHold and SharedOutbox delivery modes.
// Messages are processed concurrently up to MaxInFlight, and an
// optional global semaphore provides host-level throttling across routes.
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
	tracer      ports.Tracer
	logger      *slog.Logger
	sem         chan struct{}
	globalSem   chan struct{}
	depthCache  *outboxDepthCache
}

// RouteRunnerConfig holds the configuration for a RouteRunner.
type RouteRunnerConfig struct {
	RouteID       string
	Policy        domain.RoutePolicy
	Receiver      ports.Receiver
	Sender        ports.Sender
	OutboxStore   ports.OutboxStore
	DLQ           *DLQRouter
	Resolver      ports.DestinationResolver
	Processors    []ports.Processor
	Bindings      []domain.DestinationBinding
	InstanceID    string
	Metrics       ports.MetricsExporter
	Tracer        ports.Tracer
	Logger        *slog.Logger
	GlobalSem     chan struct{}
	DepthCacheTTL time.Duration
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
	t := cfg.Tracer
	if t == nil {
		t = &ports.NoopTracer{}
	}
	policy := cfg.Policy.WithDefaults()

	var dc *outboxDepthCache
	if policy.DeliveryMode == domain.DeliverySharedOutbox {
		depthTTL := cfg.DepthCacheTTL
		if depthTTL <= 0 {
			depthTTL = domain.DefaultDepthCacheTTL
		}
		dc = newOutboxDepthCache(depthTTL)
	}

	r := &RouteRunner{
		routeID:     cfg.RouteID,
		policy:      policy,
		receiver:    cfg.Receiver,
		sender:      cfg.Sender,
		outboxStore: cfg.OutboxStore,
		dlq:         dlq,
		resolver:    cfg.Resolver,
		processors:  cfg.Processors,
		bindings:    cfg.Bindings,
		instanceID:  cfg.InstanceID,
		metrics:     m,
		tracer:      t,
		logger:      cfg.Logger,
		globalSem:   cfg.GlobalSem,
		depthCache:  dc,
	}
	if r.policy.MaxInFlight > 0 {
		r.sem = make(chan struct{}, r.policy.MaxInFlight)
	}
	return r
}

// Run starts the receiver and processes deliveries concurrently up to
// MaxInFlight. It blocks until the context is cancelled or the receiver
// returns an unrecoverable error. In-flight goroutines are awaited on exit.
func (r *RouteRunner) Run(ctx context.Context) error {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		closed bool
	)

	err := r.receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		if err := r.acquireSlots(ctx); err != nil {
			return err
		}
		mu.Lock()
		if closed {
			mu.Unlock()
			r.releaseSlots()
			return ctx.Err()
		}
		wg.Add(1)
		mu.Unlock()
		go func() {
			defer wg.Done()
			defer r.releaseSlots()
			defer func() {
				if rec := recover(); rec != nil {
					stack := goruntime.Stack()
					if r.logger != nil {
						r.logger.Error("panic in delivery goroutine",
							"route", r.routeID,
							"panic", rec,
							"stack", string(stack),
						)
					}
					func() {
						defer func() {
							if r2 := recover(); r2 != nil && r.logger != nil {
								r.logger.Error("panic in recovery handler",
									"route", r.routeID, "panic", r2)
							}
						}()
						r.metrics.Counter(domain.MetricDeliveryPanics, 1,
							domain.Tag{Key: domain.TagKeyRouteID, Value: r.routeID},
						)
						retryErr := del.Retry(context.Background(), 0, fmt.Errorf("panic recovered in route %s: %v", r.routeID, rec))
						if retryErr != nil && r.logger != nil {
							r.logger.Error("retry after panic failed",
								"route", r.routeID, "error", retryErr)
						}
					}()
				}
			}()
			r.processDelivery(ctx, del)
		}()
		return nil
	})

	mu.Lock()
	closed = true
	mu.Unlock()
	wg.Wait()

	return err
}

// handleDelivery is the synchronous entry point used by Runtime.Inject.
func (r *RouteRunner) handleDelivery(ctx context.Context, del ports.Delivery) error {
	if err := r.acquireSlots(ctx); err != nil {
		return err
	}
	defer r.releaseSlots()
	return r.doHandleDelivery(ctx, del)
}

func (r *RouteRunner) processDelivery(ctx context.Context, del ports.Delivery) {
	if err := r.doHandleDelivery(ctx, del); err != nil {
		if r.logger != nil && ctx.Err() == nil {
			r.logger.Warn("delivery processing error",
				"route", r.routeID, "error", err)
		}
	}
}

func (r *RouteRunner) doHandleDelivery(ctx context.Context, del ports.Delivery) error {
	start := time.Now()

	env := del.Envelope()

	env.Headers = domain.StripReservedHeaders(env.Headers)
	r.injectHeaders(env)

	tc, hasTrace := domain.ExtractTraceContext(env.Headers)

	attrs := []domain.Tag{
		{Key: domain.TagKeyRouteID, Value: r.routeID},
		{Key: "envelope_id", Value: env.ID},
	}
	if hasTrace {
		attrs = append(attrs, domain.Tag{Key: "trace_id", Value: tc.TraceID})
	}

	ctx, span := r.tracer.StartSpan(ctx, "bridge.handleDelivery", attrs...)
	defer span.End()

	if corrID, ok := domain.GetHeaderString(env.Headers, domain.HeaderCorrelationID); ok {
		ctx = observability.WithCorrelationID(ctx, corrID)
	}
	if hasTrace {
		ctx = observability.WithTraceID(ctx, tc.TraceID)
		ctx = observability.WithSpanID(ctx, tc.SpanID)
	}

	if env.IsExpired() {
		err := r.handleExpired(ctx, del, env)
		if err != nil {
			span.SetError(err)
		}
		return err
	}

	if err := RunChain(ctx, r.processors, env); err != nil {
		pErr := r.handleProcessorError(ctx, del, env, err)
		if !errors.Is(err, domain.ErrMessageFiltered) {
			span.SetError(err)
		}
		return pErr
	}

	var deliveryErr error
	switch r.policy.DeliveryMode {
	case domain.DeliverySharedOutbox:
		deliveryErr = r.sharedOutbox(ctx, del, env)
	default:
		deliveryErr = r.directHold(ctx, del, env)
	}

	if deliveryErr != nil {
		span.SetError(deliveryErr)
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

	if len(plans) == 0 {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("resolver returned no dispatch plans for route %s", r.routeID))
	}

	plan := plans[0]
	if plan.Address != "" {
		env.Subject = plan.Address
	}
	if plan.Headers != nil {
		env.Headers = domain.MergeHeaders(env.Headers, plan.Headers, true)
	}

	sendCtx, sendCancel := context.WithTimeout(ctx, r.policy.SendTimeout)
	defer sendCancel()

	sendErr := r.sender.Send(sendCtx, env)
	if sendErr == nil {
		return del.Ack(ctx)
	}

	if domain.IsRecoverableError(sendErr) {
		retryAfter := domain.GetRetryAfter(sendErr)
		return r.retryOrFallback(ctx, del, env, retryAfter, sendErr)
	}

	if dlqErr := r.dlq.Route(ctx, env, r.routeID, plan.BindingID, r.sessionIDForBinding(plan.BindingID), "", sendErr, 0); dlqErr != nil {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
	}
	r.emitDLQ("permanent")
	return del.Ack(ctx)
}

func (r *RouteRunner) sharedOutbox(ctx context.Context, del ports.Delivery, env *domain.Envelope) error {
	plans, err := r.resolvePlans(ctx, env)
	if err != nil {
		return r.handleResolveError(ctx, del, env, err)
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
				return r.retryOrFallback(ctx, del, env, time.Second, fmt.Errorf("outbox depth query failed: %w", qErr))
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
			return del.Ack(ctx)
		}
		return r.retryOrFallback(ctx, del, env, 0, persistErr)
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
			return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("DLQ write failed: %w", dlqErr))
		}
		r.emitDLQ("expired")
	}
	return del.Ack(ctx)
}

func (r *RouteRunner) handleProcessorError(ctx context.Context, del ports.Delivery, env *domain.Envelope, err error) error {
	if errors.Is(err, domain.ErrMessageFiltered) {
		return del.Ack(ctx)
	}
	if domain.IsRecoverableError(err) {
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
	if !r.dlq.HasStore() && r.logger != nil {
		r.logger.Warn("message dropped: retry unsupported and no DLQ configured",
			"route", r.routeID, "envelope_id", env.ID)
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

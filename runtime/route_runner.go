package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	goruntime "runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/observability"
	"github.com/mariotoffia/gobridge/ports"
)

// RouteRunner executes the ingress pipeline for a single route.
// It supports both DirectHold and SharedOutbox delivery modes.
// Messages are processed concurrently up to MaxInFlight, and an
// optional global semaphore provides host-level throttling across routes.
type RouteRunner struct {
	routeID              string
	policy               domain.RoutePolicy
	receiver             ports.Receiver
	sender               ports.Sender
	senders              map[string]ports.Sender // binding ID -> sender (optional)
	outboxStore          ports.OutboxStore
	dlq                  *DLQRouter
	resolver             ports.DestinationResolver
	processors           []ports.Processor
	bindings             []domain.DestinationBinding
	instanceID           string
	metrics              ports.MetricsExporter
	tracer               ports.Tracer
	hook                 ports.DeliveryHook
	logger               *slog.Logger
	clk                  clock.Clock
	sem                  chan struct{}
	globalSem            chan struct{}
	depthCache           *outboxDepthCache
	panicRetryTimeout    time.Duration
	receiverCloseTimeout time.Duration
	onDelivery           func(env *domain.Envelope, err error)
	onAck                func(env *domain.Envelope, err error)
	started              chan struct{}
	startedOnce          sync.Once
	inFlight             atomic.Int64
	idleMu               sync.Mutex
	idleCh               chan struct{}
}

// RouteRunnerConfig holds the configuration for a RouteRunner.
type RouteRunnerConfig struct {
	RouteID              string
	Policy               domain.RoutePolicy
	Receiver             ports.Receiver
	Sender               ports.Sender
	Senders              map[string]ports.Sender // binding ID -> sender (optional)
	OutboxStore          ports.OutboxStore
	DLQ                  *DLQRouter
	Resolver             ports.DestinationResolver
	Processors           []ports.Processor
	Bindings             []domain.DestinationBinding
	InstanceID           string
	Metrics              ports.MetricsExporter
	Tracer               ports.Tracer
	Hook                 ports.DeliveryHook
	Logger               *slog.Logger
	GlobalSem            chan struct{}
	DepthCacheTTL        time.Duration
	PanicRetryTimeout    time.Duration
	ReceiverCloseTimeout time.Duration
	Clock                clock.Clock
	// OnDelivery is invoked (non-blocking) for each envelope the RouteRunner
	// has successfully dispatched (sent to the target). Receives the envelope
	// and any error from the send pipeline (nil on success). Optional.
	OnDelivery func(env *domain.Envelope, err error)
	// OnAck is invoked (non-blocking) after the source delivery is acked or
	// retried. Receives the envelope and the ack error (nil on successful ack).
	// Optional.
	OnAck func(env *domain.Envelope, err error)
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
	h := cfg.Hook
	if h == nil {
		h = ports.NoopDeliveryHook{}
	}
	policy := cfg.Policy.WithDefaults()

	panicRetry := cfg.PanicRetryTimeout
	if panicRetry <= 0 {
		panicRetry = 5 * time.Second
	}
	recvClose := cfg.ReceiverCloseTimeout
	if recvClose <= 0 {
		recvClose = 10 * time.Second
	}

	clk := cfg.Clock
	if clk == nil {
		clk = clock.System
	}

	var dc *outboxDepthCache
	if policy.DeliveryMode == domain.DeliverySharedOutbox {
		depthTTL := cfg.DepthCacheTTL
		if depthTTL <= 0 {
			depthTTL = domain.DefaultDepthCacheTTL
		}
		dc = newOutboxDepthCache(depthTTL, clk)
	}

	r := &RouteRunner{
		routeID:              cfg.RouteID,
		policy:               policy,
		receiver:             cfg.Receiver,
		sender:               cfg.Sender,
		senders:              cfg.Senders,
		outboxStore:          cfg.OutboxStore,
		dlq:                  dlq,
		resolver:             cfg.Resolver,
		processors:           cfg.Processors,
		bindings:             cfg.Bindings,
		instanceID:           cfg.InstanceID,
		metrics:              m,
		tracer:               t,
		hook:                 h,
		logger:               cfg.Logger,
		clk:                  clk,
		globalSem:            cfg.GlobalSem,
		depthCache:           dc,
		panicRetryTimeout:    panicRetry,
		receiverCloseTimeout: recvClose,
		onDelivery:           cfg.OnDelivery,
		onAck:                cfg.OnAck,
		started:              make(chan struct{}),
		idleCh:               make(chan struct{}),
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
	r.startedOnce.Do(func() { close(r.started) })

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
			if err := ctx.Err(); err != nil {
				return err
			}
			return errors.New("route runner: callback invoked after receiver stopped")
		}
		wg.Add(1)
		mu.Unlock()
		r.metrics.Counter(shared.MetricMessagesReceived, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
		go func() {
			r.inFlight.Add(1)
			defer func() {
				// Why: fire IdleChanged on every InFlight → 0 transition
				// so Runtime.WaitQuiescent (runtime/bridge_health.go) can
				// wake event-driven instead of polling. The signal is also
				// covered by route_runner_idle_test.go.
				if r.inFlight.Add(-1) == 0 {
					r.fireIdle()
				}
			}()
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
						r.metrics.Counter(shared.MetricDeliveryPanics, 1,
							shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID},
						)
						// Propagate caller ctx for trace/correlation values and to
						// honour any deadline the caller already set. If the caller
						// ctx is cancelled, strip cancellation but keep values so
						// the retry (ack/nack to the source) can still complete.
						var retryParent context.Context
						if ctx.Err() != nil {
							retryParent = context.WithoutCancel(ctx)
						} else {
							retryParent = ctx
						}
						retryCtx, retryCancel := context.WithTimeout(retryParent, r.panicRetryTimeout)
						retryErr := r.retryDelivery(retryCtx, del, 0, fmt.Errorf("panic recovered in route %s: %v", r.routeID, rec))
						retryCancel()
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

	// Close the receiver even when the caller ctx is already cancelled
	// (which is the common shutdown case). Preserve values for log/trace
	// correlation via WithoutCancel.
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), r.receiverCloseTimeout)
	defer closeCancel()
	if closer, ok := r.receiver.(interface{ Close(context.Context) error }); ok {
		_ = closer.Close(closeCtx)
	}

	return err
}

// Started returns a channel that is closed when the route runner's Run
// method has been entered. Callers can select on this to detect readiness.
func (r *RouteRunner) Started() <-chan struct{} { return r.started }

// InFlight returns the number of delivery goroutines currently executing.
func (r *RouteRunner) InFlight() int64 { return r.inFlight.Load() }

// IdleChanged returns a channel that closes on the next transition of
// InFlight to zero. A fresh channel is allocated on every transition,
// so callers must re-read it each iteration. Capture the channel
// before checking InFlight to avoid lost-wakeup races — see
// OutboxDrainer.WaitIdle for the template pattern.
func (r *RouteRunner) IdleChanged() <-chan struct{} {
	r.idleMu.Lock()
	defer r.idleMu.Unlock()
	return r.idleCh
}

// fireIdle closes the current idleCh and swaps in a fresh one. Called
// when inFlight transitions to zero (see the Add(-1)==0 site in Run).
func (r *RouteRunner) fireIdle() {
	r.idleMu.Lock()
	old := r.idleCh
	r.idleCh = make(chan struct{})
	r.idleMu.Unlock()
	close(old)
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
		if logging.DebugEnabled(r.logger) && ctx.Err() == nil {
			env := del.Envelope()
			r.logger.Log(ctx, logging.LevelDebug, "delivery error detail",
				"route", r.routeID,
				"envelope_id", env.ID,
				"subject", env.Subject,
				"delivery_mode", string(r.policy.DeliveryMode),
				"error", err,
			)
		}
	}
}

func (r *RouteRunner) doHandleDelivery(ctx context.Context, del ports.Delivery) error {
	start := r.clk.Now()

	env := del.Envelope()

	env.Headers = domain.StripReservedHeaders(env.Headers)
	r.injectHeaders(env)

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "delivery received",
			"route", r.routeID,
			"envelope_id", env.ID,
			"subject", env.Subject,
		)
	}

	tc, hasTrace := domain.ExtractTraceContext(env.Headers)

	attrs := []shared.Tag{
		{Key: shared.TagKeyRouteID, Value: r.routeID},
		{Key: "envelope_id", Value: env.ID},
	}
	if hasTrace {
		attrs = append(attrs, shared.Tag{Key: "trace_id", Value: tc.TraceID})
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

	r.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionIngress,
		RouteID:     r.routeID,
		Envelope:    env,
		Attempt:     1,
		MaxAttempts: r.policy.MaxReplayAttempts,
	})

	if env.IsExpired(r.clk) {
		err := r.handleExpired(ctx, del, env)
		if err != nil {
			span.SetError(err)
			return fmt.Errorf("runtime: route-runner: handle expired: %w", err)
		}
		return nil
	}

	if err := RunChain(ctx, r.processors, env,
		WithChainLogger(r.logger),
		WithChainMetrics(r.metrics),
		WithChainTimeout(r.policy.ProcessorTimeout),
		WithChainRouteID(r.routeID),
	); err != nil {
		pErr := r.handleProcessorError(ctx, del, env, err)
		if !errors.Is(err, shared.ErrMessageFiltered) {
			span.SetError(err)
		}
		return pErr
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "processors complete",
			"route", r.routeID,
			"envelope_id", env.ID,
		)
	}

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "dispatching",
			"route", r.routeID,
			"mode", string(r.policy.DeliveryMode),
		)
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

	routeTag := shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID}
	r.metrics.Timer(shared.MetricDeliveryE2ELatency, r.clk.Since(start), routeTag)

	return deliveryErr
}

func (r *RouteRunner) directHold(ctx context.Context, del ports.Delivery, env *domain.Envelope) error {
	// Consume HeaderRouteOverride set by processor chain (e.g., filter ActionRoute).
	// SEC-1: validate the override references a binding declared on this route.
	if override, ok := domain.GetHeaderString(env.Headers, domain.HeaderRouteOverride); ok {
		delete(env.Headers, domain.HeaderRouteOverride)
		if r.hasBinding(override) {
			return r.sendDirectHoldForBinding(ctx, del, env, override)
		}
		if r.logger != nil {
			r.logger.Warn("route override references unknown binding",
				"route", r.routeID, "override", override)
		}
		// Fall through to normal resolution if override binding not found.
	}

	plans, resolveErr := r.resolvePlans(ctx, env)
	if resolveErr != nil {
		return r.handleResolveError(ctx, del, env, resolveErr)
	}

	if len(plans) == 0 {
		return r.retryOrFallback(ctx, del, env, 0, fmt.Errorf("resolver returned no dispatch plans for route %s", r.routeID))
	}

	return r.sendDirectHold(ctx, del, env, plans[0])
}

package route

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	goruntime "runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/observability"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/outbox"
)

// RouteRunner executes the ingress pipeline for a single route.
// It supports both DirectHold and SharedOutbox delivery modes.
// Messages are processed concurrently up to MaxInFlight, and an
// optional global semaphore provides host-level throttling across routes.
type RouteRunner struct {
	routeID              string
	policy               routing.RoutePolicy
	sourceTransport      string
	receiver             ports.Receiver
	sender               ports.Sender
	senders              map[string]ports.Sender           // binding ID -> sender (optional)
	addressValidators    map[string]ports.AddressValidator // binding ID -> validator (optional)
	outboxStore          ports.OutboxStore
	dlq                  *dlq.Router
	resolver             ports.DestinationResolver
	processors           []ports.Processor
	bindings             []routing.DestinationBinding
	instanceID           string
	metrics              ports.MetricsExporter
	tracer               ports.Tracer
	hook                 ports.DeliveryHook
	logger               *slog.Logger
	clk                  clock.Clock
	sem                  chan struct{}
	globalSem            chan struct{}
	depthCache           *outbox.DepthCache
	panicRetryTimeout    time.Duration
	receiverCloseTimeout time.Duration
	onDelivery           func(env *messaging.Envelope, err error)
	onAck                func(env *messaging.Envelope, err error)
	started              chan struct{}
	startedOnce          sync.Once
	inFlight             atomic.Int64
	idleMu               sync.Mutex
	idleCh               chan struct{}
}

// RouteRunnerConfig holds the configuration for a RouteRunner.
type RouteRunnerConfig struct {
	RouteID string
	Policy  routing.RoutePolicy
	// SourceTransport is the identity of the transport that FEEDS this route
	// (the RegisterTransportFactory name the operator declared, or the adapter's
	// canonical PluginConfig.Kind). It drives the ingress redelivery-count strip
	// (stripForeignReceiveCounts): the receiving transport keeps only its own
	// native count header and drops any foreign one an untrusted producer forged
	// (F3). Empty disables the strip (legacy behaviour for callers that construct
	// a runner without a declared source transport). Optional.
	SourceTransport      string
	Receiver             ports.Receiver
	Sender               ports.Sender
	Senders              map[string]ports.Sender           // binding ID -> sender (optional)
	AddressValidators    map[string]ports.AddressValidator // binding ID -> validator (optional)
	OutboxStore          ports.OutboxStore
	DLQ                  *dlq.Router
	Resolver             ports.DestinationResolver
	Processors           []ports.Processor
	Bindings             []routing.DestinationBinding
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
	OnDelivery func(env *messaging.Envelope, err error)
	// OnAck is invoked (non-blocking) after the source delivery is acked or
	// retried. Receives the envelope and the ack error (nil on successful ack).
	// Optional.
	OnAck func(env *messaging.Envelope, err error)
}

// NewRouteRunnerFromConfig creates a RouteRunner from a config struct.
func NewRouteRunnerFromConfig(cfg RouteRunnerConfig) *RouteRunner {
	return newRouteRunner(cfg)
}

// applyDefaults fills zero-valued / nil fields with their runtime
// defaults so newRouteRunner can read the config straight through
// without inline default-fill branches. It mutates c in place.
func (c *RouteRunnerConfig) applyDefaults() {
	if c.DLQ == nil {
		c.DLQ = dlq.New(nil)
	}
	if c.Metrics == nil {
		c.Metrics = &ports.NoopExporter{}
	}
	if c.Tracer == nil {
		c.Tracer = &ports.NoopTracer{}
	}
	if c.Hook == nil {
		c.Hook = ports.NoopDeliveryHook{}
	}
	if c.PanicRetryTimeout <= 0 {
		c.PanicRetryTimeout = 5 * time.Second
	}
	if c.ReceiverCloseTimeout <= 0 {
		c.ReceiverCloseTimeout = 10 * time.Second
	}
	if c.Clock == nil {
		c.Clock = clock.System
	}
	c.Policy = c.Policy.WithDefaults()
	if c.Policy.DeliveryMode == routing.DeliverySharedOutbox && c.DepthCacheTTL <= 0 {
		c.DepthCacheTTL = routing.DefaultDepthCacheTTL
	}
}

func newRouteRunner(cfg RouteRunnerConfig) *RouteRunner {
	cfg.applyDefaults()

	var dc *outbox.DepthCache
	if cfg.Policy.DeliveryMode == routing.DeliverySharedOutbox {
		dc = outbox.NewDepthCache(cfg.DepthCacheTTL, cfg.Clock)
	}

	r := &RouteRunner{
		routeID:              cfg.RouteID,
		policy:               cfg.Policy,
		sourceTransport:      cfg.SourceTransport,
		receiver:             cfg.Receiver,
		sender:               cfg.Sender,
		senders:              cfg.Senders,
		addressValidators:    cfg.AddressValidators,
		outboxStore:          cfg.OutboxStore,
		dlq:                  cfg.DLQ,
		resolver:             cfg.Resolver,
		processors:           cfg.Processors,
		bindings:             cfg.Bindings,
		instanceID:           cfg.InstanceID,
		metrics:              cfg.Metrics,
		tracer:               cfg.Tracer,
		hook:                 cfg.Hook,
		logger:               cfg.Logger,
		clk:                  cfg.Clock,
		globalSem:            cfg.GlobalSem,
		depthCache:           dc,
		panicRetryTimeout:    cfg.PanicRetryTimeout,
		receiverCloseTimeout: cfg.ReceiverCloseTimeout,
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
		// Finding 16: count in-flight BEFORE spawning the goroutine. If we
		// incremented inside the goroutine, WaitQuiescent could snapshot 0
		// between "emit accepted the delivery" and "goroutine started",
		// reporting quiescence with an accepted-but-unstarted delivery.
		r.inFlight.Add(1)
		// Finding 18: wrap the delivery so terminal settlement (Ack/Retry) is
		// observable from the panic-recovery path — a panic AFTER settlement
		// must not trigger a duplicate retry on an already-settled delivery.
		tracked := &settleTrackingDelivery{Delivery: del}
		go func() {
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
						// Finding 18: a panic AFTER the delivery already reached a
						// terminal state (e.g. in a tracer/metric/hook call fired
						// after ACK) must not re-settle it — retrying an acked
						// delivery is duplicate noise.
						if tracked.Settled() {
							if r.logger != nil {
								r.logger.Warn("panic after delivery settled; skipping retry",
									"route", r.routeID)
							}
							return
						}
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
						defer retryCancel()
						// Finding 2: panics outside RunChain (resolver, hooks,
						// tracer, metrics) reach here. Route the recovery through
						// the SAME receive-count poison gate the send path uses so
						// a deterministically-panicking resolver cannot spin an
						// uncapped, immediate redelivery hot loop.
						r.recoverDelivery(retryCtx, tracked,
							fmt.Errorf("panic recovered in route %s: %v", r.routeID, rec))
					}()
				}
			}()
			r.processDelivery(ctx, tracked)
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

// Receiver returns the configured ingress receiver. Exposed so the
// parent runtime's health aggregator can probe transport-level
// readiness (via [ports.ReceiverStartedSignaler]) without reaching
// into the route package's internals.
func (r *RouteRunner) Receiver() ports.Receiver { return r.receiver }

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

// bindingOverrider is an out-of-band, trusted channel for a binding-scoped
// route override. Only internally-constructed synthetic deliveries (the admin
// DLQ redrive via Runtime.InjectToBinding) implement it; transport-created
// deliveries never do. doHandleDelivery re-stamps HeaderRouteOverride from this
// source AFTER the ingress reserved-header strip, so an external message can
// never steer routing through x-bridge.route-override (the strip removes any it
// carried), while an operator redriving a single failed fan-out leg still
// confines the replay to that one binding.
type bindingOverrider interface {
	// BindingOverride returns the binding ID the delivery must be confined to,
	// or "" when no override applies.
	BindingOverride() string
}

// redriveProvenancer is the out-of-band, trusted channel for DLQ-redrive
// provenance. Only internally-constructed synthetic deliveries
// (Runtime.InjectRedrive) implement it; transport-created deliveries never do.
// A redriven message is re-issued under a FRESH envelope ID (reusing the
// original would collide with the outbox's retained dedup row — see
// Runtime.InjectRedrive), so the original ID is stamped as
// messaging.HeaderCausationID AFTER the ingress reserved-header strip below.
// An external message can never spoof it: the strip removes any inbound
// x-bridge.causation-id on untrusted routes before this re-stamp runs.
type redriveProvenancer interface {
	// RedrivenFrom returns the original envelope ID the delivery re-issues,
	// or "" when the delivery is not a redrive.
	RedrivenFrom() string
}

// HandleDelivery is the synchronous entry point used by Runtime.Inject.
func (r *RouteRunner) HandleDelivery(ctx context.Context, del ports.Delivery) error {
	if err := r.acquireSlots(ctx); err != nil {
		return err
	}
	defer r.releaseSlots()
	// Count a synchronous inject in the SAME in-flight accounting the Run
	// receive loop uses (r.inFlight + fireIdle), so Runtime.InFlight() /
	// WaitQuiescent observes an in-progress admin inject/redrive and a reconfig
	// cannot declare the route quiescent mid-redrive. Increment BEFORE
	// processing; fire the idle signal on the count→0 transition exactly as the
	// Run loop does.
	r.inFlight.Add(1)
	defer func() {
		if r.inFlight.Add(-1) == 0 {
			r.fireIdle()
		}
	}()
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
				"envelope_id", env.ID(),
				"subject", env.Subject(),
				"delivery_mode", string(r.policy.DeliveryMode),
				"error", err,
			)
		}
	}
}

func (r *RouteRunner) doHandleDelivery(ctx context.Context, del ports.Delivery) error {
	start := r.clk.Now()

	env := del.Envelope()

	// Confine an OUT-OF-BAND binding override (an operator DLQ redrive via
	// Runtime.InjectToBinding / InjectRedrive, carried on a runtime-internal
	// bindingOverrider delivery) to the ONE recorded binding. If that binding no
	// longer exists on the (possibly reconfigured) route, reject the replay as a
	// PERMANENT not-found BEFORE it enters the pipeline — emitting no
	// MessagesReceived and running no chain/dispatch — so the admin DLQ redrive
	// path restores/keeps the DLQ entry instead of fanning the replay out to
	// every current binding (duplicate deliveries to the N-1 healthy legs). This
	// is deliberately distinct from an IN-BAND processor override (a filter
	// ActionRoute stamps HeaderRouteOverride during the chain), whose
	// unknown-binding fall-through to normal resolution in directHold /
	// sharedOutbox is intentional and separately tested. Only the out-of-band
	// origin — a delivery implementing bindingOverrider, constructed inside the
	// runtime and never by a transport adapter — is confined here.
	if bo, ok := del.(bindingOverrider); ok {
		if bid := bo.BindingOverride(); bid != "" && !r.hasBinding(bid) {
			return shared.ErrNotFound.WithMessage(
				fmt.Sprintf("runtime: route-runner: route %q: redrive binding %q not found", r.routeID, bid))
		}
	}

	// Finding 15: count every delivery that ENTERS the pipeline here — the sole
	// choke point shared by both the Run receive loop and Runtime.Inject
	// (HandleDelivery). Previously only the Run callback emitted it, so injected
	// deliveries were invisible to the conservation law even though they emit
	// MessagesSent on success. Counting here (not in the Run callback) keeps a
	// single emission per delivery across both entry points.
	r.metrics.Counter(shared.MetricMessagesReceived, 1,
		shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})

	// Ingress reserved-header handling. The DEFAULT posture strips ALL
	// x-bridge.* headers so an external producer can never inject bridge
	// metadata (route steering, spoofed tenant/correlation).
	//
	// TrustBridgeHeaders relaxes this for a receiver fed exclusively by a
	// trusted upstream bridge: the BRIDGE-TO-BRIDGE PROPAGATED class
	// (correlation-id, causation-id, idempotency-key, dedup-id, ordering-key,
	// tenant-id, forwarded-from/hop) survives the hop so cross-bridge
	// correlation/idempotency is preserved. It MUST use StampHeaders (the
	// trusted whole-map setter) — ReplaceHeaders re-strips every reserved key
	// and would defeat the point. INTERNAL-ONLY headers (route-id,
	// route-override, source-id, content-type) are stripped in BOTH modes via
	// StripInternalOnlyHeaders, so a trusted-header route still cannot be
	// steered by an inbound x-bridge.route-override; the bindingOverrider
	// re-stamp below remains the only path that (re)introduces one. (W3C trace
	// context — traceparent/tracestate — is not x-bridge.*-prefixed, is never
	// stripped by StripReservedHeaders, and survives in BOTH modes.)
	if r.policy.TrustBridgeHeaders {
		env.StampHeaders(messaging.StripInternalOnlyHeaders(env.HeadersSnapshot()))
	} else {
		env.ReplaceHeaders(messaging.StripReservedHeaders(env.Headers()))
	}
	// Ingress redelivery-count sanitization (F3). The transport-namespaced count
	// keys (sqs.ApproximateReceiveCount, asb.delivery-count, amqp10.delivery-count)
	// are NOT x-bridge.*-reserved, so the strip above leaves them in place. An
	// untrusted producer on a count-less source (MQTT copies arbitrary user
	// properties into headers) could forge one and have its first delivery read as
	// over the replay cap. Keep only THIS source transport's own count key and
	// drop the foreign ones BEFORE any receiveCount read on the retry/poison path.
	stripForeignReceiveCounts(env, r.sourceTransport)
	r.injectHeaders(env)

	// Re-stamp a binding-scoped route override carried out-of-band by a trusted
	// synthetic delivery (operator DLQ redrive via Runtime.InjectToBinding). The
	// StripReservedHeaders call above removed any x-bridge.route-override an
	// external payload may have carried, so external messages can never steer
	// routing; only a delivery that implements bindingOverrider — constructed
	// inside the runtime, never by a transport adapter — can re-introduce it. The
	// stamp precedes the chain clone below so it flows through the processor
	// chain and survives mergeProcessedEnvelope onto the dispatch envelope, where
	// directHold / sharedOutbox consume it and confine the replay to that one
	// binding (no duplicate deliveries to the N-1 healthy fan-out legs).
	if bo, ok := del.(bindingOverrider); ok {
		if bid := bo.BindingOverride(); bid != "" {
			env.SetHeader(messaging.HeaderRouteOverride, bid)
		}
	}

	// Stamp DLQ-redrive provenance (the ORIGINAL envelope ID this delivery
	// re-issues under a fresh ID) after the strip, for the same trust reason
	// as the binding override above. HeaderCausationID is BRIDGE-TO-BRIDGE
	// PROPAGATED, so the audit link survives persistence and downstream hops.
	if rp, ok := del.(redriveProvenancer); ok {
		if from := rp.RedrivenFrom(); from != "" {
			env.SetHeader(messaging.HeaderCausationID, from)
		}
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "delivery received",
			"route", r.routeID,
			"envelope_id", env.ID(),
			"subject", env.Subject(),
		)
	}

	tc, hasTrace := messaging.ExtractTraceContext(env.Headers())

	attrs := []shared.Tag{
		{Key: shared.TagKeyRouteID, Value: r.routeID},
		{Key: "envelope_id", Value: env.ID()},
	}
	if hasTrace {
		attrs = append(attrs, shared.Tag{Key: "trace_id", Value: tc.TraceID})
	}

	// Join the upstream trace before creating this hop's span: Extract
	// parents the runtime span on the remote span carried by W3C
	// traceparent, so the bridge appears as a child of the caller (K1).
	ctx = r.tracer.Extract(ctx, env.Headers())

	ctx, span := r.tracer.StartSpan(ctx, "bridge.handleDelivery", attrs...)
	defer span.End()

	if corrID, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationID); ok {
		ctx = observability.WithCorrelationID(ctx, corrID)
	}
	// Log-correlation fields come from the ACTIVE span when the tracer
	// exposes its identity (ports.SpanIdentity): logs then carry THIS
	// hop's span_id, and root deliveries get a trace_id at all. The
	// upstream traceparent stays the span parent (Extract above) and
	// span attribute; it is only the stamping fallback for tracers
	// without the capability (NoopTracer) or unsampled traces.
	stamped := false
	if ident, ok := span.(ports.SpanIdentity); ok {
		if tid := ident.TraceID(); tid != "" {
			ctx = observability.WithTraceID(ctx, tid)
			if sid := ident.SpanID(); sid != "" {
				ctx = observability.WithSpanID(ctx, sid)
			}
			stamped = true
		}
	}
	if !stamped && hasTrace {
		ctx = observability.WithTraceID(ctx, tc.TraceID)
		ctx = observability.WithSpanID(ctx, tc.SpanID)
	}

	// Install a delivery-scoped release registry so a mid-chain processor can
	// bracket a resource across the WHOLE delivery (incl. the egress send in
	// direct_hold / the outbox persist in shared_outbox). Generic: the runtime
	// knows nothing of what is registered.
	ctx, scope := ports.WithDeliveryScope(ctx)
	defer scope.Release()

	// Attempt reflects the source transport's redelivery count when present
	// (e.g. SQS ApproximateReceiveCount): a transport redelivery is attempt 2+,
	// not attempt 1. receiveCount is 1-based (1 == first delivery) and returns 0
	// when no known counter header is present, so fall back to 1.
	ingressAttempt := receiveCount(env)
	if ingressAttempt < 1 {
		ingressAttempt = 1
	}
	r.hook.OnAttempt(ctx, ports.DeliveryAttempt{
		Direction:   ports.DirectionIngress,
		RouteID:     r.routeID,
		Envelope:    env,
		Attempt:     ingressAttempt,
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

	// chainEnv is an isolated clone the processor chain mutates. On a chain
	// TIMEOUT or shutdown-grace abandonment (route/chain.go) the underlying
	// processor goroutine keeps running with a *live* envelope; were that the
	// shared source envelope, a late SetHeader would race the runner's
	// concurrent reads (receiveCount, DLQ serialization) — a fatal concurrent
	// map read/write. Running the chain on a clone confines any
	// abandoned-goroutine mutation to chainEnv; the source env is only ever
	// read by the runner on the error path. On SUCCESS every processor
	// goroutine has returned (happens-before close(done) in invokeProcessor),
	// so the clone is quiescent and we merge its processor-owned mutations
	// (subject, payload, headers) back onto env for dispatch.
	chainEnv := env.Clone()
	if err := RunChain(ctx, r.processors, chainEnv,
		WithChainLogger(r.logger),
		WithChainMetrics(r.metrics),
		WithChainTimeout(r.policy.ProcessorTimeout),
		WithChainRouteID(r.routeID),
		WithChainClock(r.clk),
	); err != nil {
		pErr := r.handleProcessorError(ctx, del, env, err)
		if !errors.Is(err, shared.ErrMessageFiltered) {
			span.SetError(err)
		}
		return pErr
	}
	mergeProcessedEnvelope(env, chainEnv)

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "processors complete",
			"route", r.routeID,
			"envelope_id", env.ID(),
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
	case routing.DeliverySharedOutbox:
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

func (r *RouteRunner) directHold(ctx context.Context, del ports.Delivery, env *messaging.Envelope) error {
	// Consume HeaderRouteOverride set by processor chain (e.g., filter ActionRoute).
	// SEC-1: validate the override references a binding declared on this route.
	if override, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderRouteOverride); ok {
		env.DeleteHeader(messaging.HeaderRouteOverride)
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

	if len(plans) > 1 {
		// direct_hold is single-dispatch: it delivers exactly one leg and ACKs
		// the source. A resolver that yielded multiple plans here would send
		// plans[0] and silently drop the remainder after the source is ACKed —
		// unrecoverable loss. The validator blocks the fan-out POLICY, but an
		// opaque resolver can still return >1 plan at runtime; refuse it as a
		// permanent config error (routed to DLQ/drop per policy) rather than
		// vanishing the extra legs.
		r.metrics.Counter(shared.MetricRouteErrors, 1,
			shared.Tag{Key: shared.TagKeyRouteID, Value: r.routeID})
		if r.logger != nil {
			r.logger.Error("direct_hold resolver returned multiple dispatch plans; single-dispatch delivers one leg only",
				"route", r.routeID, "plan_count", len(plans), "envelope_id", env.ID())
		}
		multiErr := shared.NewBridgeError(shared.ErrCodeInternal, shared.ErrorPermanent,
			fmt.Sprintf("direct_hold route %q: resolver returned %d dispatch plans but single-dispatch delivers exactly one; refusing to silently drop the remainder (use shared_outbox for fan-out)", r.routeID, len(plans)))
		return r.handleProcessorError(ctx, del, env, multiErr)
	}

	return r.sendDirectHold(ctx, del, env, plans[0])
}

// recoverDelivery settles a delivery whose processing goroutine panicked
// OUTSIDE the processor chain (resolver, hooks, tracer, metrics — RunChain has
// its own recovery). Finding 2: it applies the SAME receive-count poison gate
// the send path uses so a deterministically-panicking resolver cannot spin an
// uncapped, immediate redelivery hot loop. At or above MaxReplayAttempts the
// message is poisoned to the DLQ (or dropped-with-metric when the policy is
// drop or no store is configured); below the cap it is retried with the
// policy's bounded backoff (never the previous immediate, zero-delay retry).
func (r *RouteRunner) recoverDelivery(ctx context.Context, del ports.Delivery, cause error) {
	env := del.Envelope()
	rc := receiveCount(env)
	attempts := rc + 1

	if r.policy.MaxReplayAttempts > 0 && rc >= r.policy.MaxReplayAttempts {
		poisonErr := shared.NewBridgeError(shared.ErrCodePoisonMessage, shared.ErrorPermanent,
			fmt.Sprintf("panic-recovery: receive count %d >= max replay attempts %d: %v", rc, r.policy.MaxReplayAttempts, cause))
		if r.policy.OnPermanentFailure == routing.FailureDrop || !r.dlq.HasStore() {
			r.emitDrop("panic")
			if r.logger != nil {
				r.logger.Warn("panic-poison dropped: at replay cap with no DLQ or drop policy",
					"route", r.routeID, "envelope_id", env.ID(), "receive_count", rc, "error", cause)
			}
			if err := r.settleTerminal(ctx, del, env, poisonErr, attempts); err != nil && r.logger != nil {
				r.logger.Error("panic-poison settle failed", "route", r.routeID, "error", err)
			}
			return
		}
		if dlqErr := r.dlq.Route(ctx, env, r.routeID, "", "", "", "", poisonErr, rc); dlqErr != nil {
			// Leave the delivery unsettled so the source redelivers rather than
			// silently dropping a poison message we could not persist.
			if r.logger != nil {
				r.logger.Error("panic-poison DLQ write failed; leaving delivery for redelivery",
					"route", r.routeID, "envelope_id", env.ID(), "error", dlqErr)
			}
			return
		}
		r.emitDLQ("panic")
		if err := r.settleTerminal(ctx, del, env, poisonErr, attempts); err != nil && r.logger != nil {
			r.logger.Error("panic-poison settle failed", "route", r.routeID, "error", err)
		}
		return
	}

	// Below the cap: bounded-delay retry, falling back to DLQ/drop when the
	// source transport does not support retry (retryOrFallback handles both).
	if err := r.retryOrFallback(ctx, del, env, RetryDelay(r.policy, attempts, cause), cause); err != nil && r.logger != nil {
		r.logger.Error("retry after panic failed", "route", r.routeID, "error", err)
	}
}

// settleTrackingDelivery wraps a ports.Delivery to record terminal settlement.
// A delivery is "settled" once Ack or Retry has succeeded. The panic-recovery
// path consults Settled() so a panic AFTER settlement (finding 18) does not
// re-settle an already-terminal delivery. Extend is a visibility operation, not
// a settlement, so it is not tracked.
//
// HAZARD (F10): embedding ports.Delivery promotes every current and FUTURE
// method of the wrapped Delivery, but a wrapper is opaque to interface probing —
// a caller doing `del.(SomeOptionalCap)` sees THIS concrete type, not the inner
// delivery, so any optional capability the underlying Delivery grows is silently
// masked. If such an optional Delivery capability is ever added, it MUST be
// explicitly forwarded here (add the method and delegate to d.Delivery, or expose
// the inner value via an Unwrap) so this wrapper does not hide it.
type settleTrackingDelivery struct {
	ports.Delivery
	settled atomic.Bool
}

func (d *settleTrackingDelivery) Ack(ctx context.Context) error {
	err := d.Delivery.Ack(ctx)
	if err == nil {
		d.settled.Store(true)
	}
	return err
}

func (d *settleTrackingDelivery) Retry(ctx context.Context, after time.Duration, reason error) error {
	err := d.Delivery.Retry(ctx, after, reason)
	if err == nil {
		d.settled.Store(true)
	}
	return err
}

// Settled reports whether the delivery has reached a terminal settlement.
func (d *settleTrackingDelivery) Settled() bool { return d.settled.Load() }

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
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Runtime is the top-level coordinator for the GoBridge message routing
// engine. It manages routes, sessions, and outbox drainers.
// Start returns immediately after spawning background goroutines.
// Stop cancels the context, waits for all goroutines to finish, then
// closes sessions.
type Runtime struct {
	instanceID        string
	clk               clock.Clock
	leaseStore        ports.LeaseStore
	outboxStore       ports.OutboxStore
	dlqStore          ports.DLQStore
	metrics           ports.MetricsExporter
	audit             ports.AuditLogger
	tracer            ports.Tracer
	hook              ports.DeliveryHook
	logger            *slog.Logger
	globalMaxInFlight int
	clusterEndpoints  map[string]string
	locator           *routeLocator

	shutdownTimeout time.Duration

	credRefresher credentialRefresher

	mu sync.Mutex
	entries         []*routeEntry
	sessionSenders  map[string]*sessionSenderEntry
	sessionMgrs     map[string]*SessionManager
	drainers        []*OutboxDrainer
	dlqRouter       *DLQRouter
	globalSem       chan struct{}
	running         bool
	healthy         bool
	componentErrors map[string]error
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

type routeEntry struct {
	config   RouteConfig
	runner   *RouteRunner
	receiver ports.Receiver
	sender   ports.Sender
	session  ports.Session
	sessCfg  *SessionConfig
}

// sessionSenderEntry pairs a session with its sender and configuration,
// allowing the runtime to create drainers for sessions that are not
// directly attached to a route entry (e.g. fan-out target sessions).
type sessionSenderEntry struct {
	config  SessionConfig
	session ports.Session
	sender  ports.Sender
}

// Option configures a Runtime.
type Option func(*Runtime)

// WithInstanceID sets the bridge instance identifier used for lease
// ownership and outbox claim tracking.
func WithInstanceID(id string) Option {
	return func(rt *Runtime) { rt.instanceID = id }
}

// WithLeaseStore sets the lease store for cluster ownership.
func WithLeaseStore(store ports.LeaseStore) Option {
	return func(rt *Runtime) { rt.leaseStore = store }
}

// WithOutboxStore sets the durable outbox store for SharedOutbox delivery.
func WithOutboxStore(store ports.OutboxStore) Option {
	return func(rt *Runtime) { rt.outboxStore = store }
}

// WithDLQStore sets the dead-letter queue store.
func WithDLQStore(store ports.DLQStore) Option {
	return func(rt *Runtime) { rt.dlqStore = store }
}

// WithMetrics sets the metrics exporter for the runtime. When set,
// the runtime emits latency, counter, and gauge metrics for leases,
// outbox operations, delivery, and DLQ events.
func WithMetrics(m ports.MetricsExporter) Option {
	return func(rt *Runtime) { rt.metrics = m }
}

// WithAuditLogger sets the audit logger for the runtime. When set,
// the runtime emits structured audit events for lease transitions,
// DLQ operations, and other security-relevant actions.
func WithAuditLogger(a ports.AuditLogger) Option {
	return func(rt *Runtime) { rt.audit = a }
}

// WithTracer sets the distributed tracer for the runtime. When set,
// the runtime starts spans around message delivery and records errors.
// Defaults to NoopTracer if not configured.
func WithTracer(t ports.Tracer) Option {
	return func(rt *Runtime) { rt.tracer = t }
}

// WithDeliveryHook sets the delivery hook for observing message lifecycle
// events. The hook is called on every ingress receive, every egress send
// attempt, and once when a message reaches its terminal state (delivered,
// DLQ'd, dropped, or expired). Defaults to NoopDeliveryHook if not set.
func WithDeliveryHook(h ports.DeliveryHook) Option {
	return func(rt *Runtime) { rt.hook = h }
}

// WithLogger sets the structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(rt *Runtime) { rt.logger = logger }
}

// WithClusterEndpoints sets the endpoints that identify this instance in the
// cluster. These are passed to LeaseStore.Acquire and LeaseStore.Renew so
// other instances can discover how to reach this one.
func WithClusterEndpoints(endpoints map[string]string) Option {
	return func(rt *Runtime) { rt.clusterEndpoints = endpoints }
}

// WithShutdownTimeout sets the timeout used during graceful shutdown for
// closing sessions and flushing metrics. Zero or negative values fall back
// to a default of 5 seconds.
func WithShutdownTimeout(d time.Duration) Option {
	return func(rt *Runtime) { rt.shutdownTimeout = d }
}

// WithClock sets the clock used by all runtime components. When nil or
// not set, the runtime defaults to clock.System (real wall-clock time).
func WithClock(c clock.Clock) Option {
	return func(rt *Runtime) {
		if c == nil {
			return
		}
		rt.clk = c
	}
}

// WithGlobalMaxInFlight sets a host-level concurrency limit that is shared
// across all routes. When set to a positive value, the runtime creates a
// shared semaphore that each RouteRunner must acquire in addition to its
// per-route MaxInFlight slot. A value of 0 (default) disables global
// throttling for backward compatibility. Negative values are clamped to 0.
func WithGlobalMaxInFlight(n int) Option {
	return func(rt *Runtime) {
		if n < 0 {
			n = 0
		}
		rt.globalMaxInFlight = n
	}
}

// New creates a new Runtime with the given options.
func New(opts ...Option) *Runtime {
	rt := &Runtime{
		instanceID:     generateID(),
		sessionSenders: make(map[string]*sessionSenderEntry),
		sessionMgrs:    make(map[string]*SessionManager),
		healthy:        true,
		audit:          ports.NoopAuditLogger{},
		tracer:         &ports.NoopTracer{},
		hook:           ports.NoopDeliveryHook{},
	}
	for _, opt := range opts {
		opt(rt)
	}
	if rt.clk == nil {
		rt.clk = clock.System
	}
	return rt
}

// Stop gracefully shuts down the runtime. It cancels all goroutines,
// waits for them to finish, then closes sessions. If ctx expires before
// goroutines finish, sessions are still closed with the expired context
// and an error is returned.
func (rt *Runtime) Stop(ctx context.Context) error {
	rt.mu.Lock()
	if !rt.running {
		rt.mu.Unlock()
		return nil
	}
	rt.running = false
	cancel := rt.cancel
	rt.mu.Unlock()

	if logging.DebugEnabled(rt.logger) {
		rt.logger.Log(ctx, logging.LevelDebug, "runtime stopping",
			"instance_id", rt.instanceID,
		)
	}

	if cancel != nil {
		cancel()
	}

	// Close credential refresher BEFORE session teardown so that a
	// rotation in flight cannot race ApplyCredentials against session
	// Close (see AttachCredentialRefresher rationale).
	rt.mu.Lock()
	cr := rt.credRefresher
	rt.credRefresher = nil
	rt.mu.Unlock()
	if cr != nil {
		cr.Close()
	}

	var errs []error

	done := make(chan struct{})
	go func() {
		rt.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}

	// Drain remaining DLQ buffer entries before closing sessions.
	if rt.dlqRouter != nil {
		rt.dlqRouter.Close()
	}

	closeTimeout := rt.shutdownTimeout
	if closeTimeout <= 0 {
		closeTimeout = 5 * time.Second
	}
	// Close/flush must complete regardless of caller ctx cancellation,
	// but we preserve values (trace/correlation) for logging.
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer closeCancel()

	rt.mu.Lock()
	for _, mgr := range rt.sessionMgrs {
		if err := mgr.Close(closeCtx); err != nil {
			errs = append(errs, err)
		}
	}
	metrics := rt.metrics
	rt.mu.Unlock()

	if metrics != nil {
		flushTimeout := rt.shutdownTimeout / 2
		if flushTimeout <= 0 {
			flushTimeout = 5 * time.Second
		}
		flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
		defer flushCancel()
		if err := metrics.Flush(flushCtx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (rt *Runtime) startBackground(ctx context.Context, name string, fn func(context.Context) error) {
	if logging.TraceEnabled(rt.logger) {
		rt.logger.Log(ctx, logging.LevelTrace, "background started", "name", name)
	}
	rt.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				stack := goruntime.Stack()
				err := fmt.Errorf("panic in %s: %v", name, r)
				if rt.logger != nil {
					rt.logger.Error("background component panicked",
						"component", name, "panic", r, "stack", string(stack))
				}
				rt.mu.Lock()
				rt.componentErrors[name] = err
				rt.healthy = false
				cancel := rt.cancel
				rt.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
		}()
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			if rt.logger != nil {
				rt.logger.Error("background component failed", "component", name, "error", err)
			}
			rt.mu.Lock()
			rt.componentErrors[name] = err
			if errors.Is(err, domain.ErrStaleFencingToken) {
				rt.mu.Unlock()
				return
			}
			rt.healthy = false
			cancel := rt.cancel
			rt.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	})
}

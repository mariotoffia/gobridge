package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	goruntime "runtime/debug"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/cluster"
	"github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/runtime/session"
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
	locator           *cluster.Locator

	shutdownTimeout time.Duration

	// randFloat supplies the [0,1) value used to jitter session-restart
	// backoff (superviseSession). Production leaves it nil and the supervisor
	// falls back to math/rand/v2.Float64; tests inject a deterministic source
	// so the jittered wait is reproducible under the fake clock.
	randFloat func() float64

	credRefresherClose func(context.Context)

	mu              sync.Mutex
	entries         []*routeEntry
	sessionSenders  map[string]*sessionSenderEntry
	sessionMgrs     map[string]*session.Manager
	drainers        []*outbox.Drainer
	globalSem       chan struct{}
	running         bool
	healthy         bool
	terminal        bool
	componentErrors map[string]error
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

type routeEntry struct {
	config   RouteConfig
	runner   *route.RouteRunner
	receiver ports.Receiver
	sender   ports.Sender
	session  ports.Session
	sessCfg  *session.Config
}

// sessionSenderEntry pairs a session with its sender and configuration,
// allowing the runtime to create drainers for sessions that are not
// directly attached to a route entry (e.g. fan-out target sessions).
type sessionSenderEntry struct {
	config  session.Config
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
		sessionMgrs:    make(map[string]*session.Manager),
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

// AttachCredentialCloser registers a close-on-stop hook with the runtime.
// The runtime invokes this closure during Stop, before session teardown,
// so any goroutines that call ApplyCredentials on a session can be
// cancelled safely. The closer receives a bounded ctx; honouring it lets
// the runtime cap Stop latency when a watcher is unresponsive.
//
// Accepting a closure (rather than an interface value) deliberately keeps
// runtime free of any structural reference to a caller-defined type:
// the runtime sees only func(context.Context); deep architecture
// analysis cannot infer a phantom dependency on the caller's package.
func (rt *Runtime) AttachCredentialCloser(close func(context.Context)) {
	if rt == nil || close == nil {
		return
	}
	rt.mu.Lock()
	rt.credRefresherClose = close
	rt.mu.Unlock()
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
	// Close (see AttachCredentialCloser rationale). The closer is
	// invoked under a bounded timeout so a stuck watcher cannot hang
	// Stop past the user-supplied ctx.
	rt.mu.Lock()
	closeRefresher := rt.credRefresherClose
	rt.credRefresherClose = nil
	rt.mu.Unlock()

	closeTimeout := rt.shutdownTimeout
	if closeTimeout <= 0 {
		closeTimeout = 5 * time.Second
	}

	if closeRefresher != nil {
		refresherCtx, refresherCancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		// Spawn the closer with explicit lifetime; if it overruns the
		// bounded timer or the caller's ctx, we move on (best-effort)
		// rather than blocking Stop.
		refresherDone := make(chan struct{})
		go func() {
			defer close(refresherDone)
			closeRefresher(refresherCtx)
		}()
		select {
		case <-refresherDone:
		case <-refresherCtx.Done():
		case <-ctx.Done():
		}
		refresherCancel()
	}

	var errs []error

	// Wait-goroutine has explicit fire-and-forget lifetime: it survives
	// only until rt.wg drains. If ctx fires first we record the error
	// and proceed to session close under closeCtx (below); the bounded
	// closeCtx ensures Stop cannot hang indefinitely waiting on a stuck
	// background goroutine.
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		rt.wg.Wait()
	}()

	select {
	case <-waitDone:
	case <-ctx.Done():
		// Pre-cancelled / early-expiring ctx: we stop waiting for background
		// goroutines and proceed to close metrics/tracer below. A straggler
		// (e.g. a drainer still finalising) may therefore emit a counter or span
		// after its provider is closed. That is benign: OTel Counter/Start calls
		// on a shut-down provider are no-ops and the SDK is concurrency-safe, so
		// no panic, race, or corruption results (OTEL-N6).
		errs = append(errs, ctx.Err())
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
	tracer := rt.tracer
	stores := []any{rt.outboxStore, rt.dlqStore, rt.leaseStore}
	rt.mu.Unlock()

	// Release store resources (e.g. SQLite file handles) after every session
	// manager has closed and drained. Stores that hold OS resources implement
	// io.Closer; in-memory stores do not and are skipped. Closing here is safe
	// because reconfiguration always builds a fresh runtime with its own store
	// instances (buildStores) before Stopping the old one, so a closed handle
	// is never shared with a live runtime; without this the old runtime's
	// handles leaked on every reload. (Finding I5.)
	for _, s := range stores {
		if c, ok := s.(io.Closer); ok {
			if err := c.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}

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
		// Close releases the exporter/provider; Flush alone does not shut the
		// metric reader down. Close under closeCtx — the same full-budget context
		// the tracer Close uses below — NOT the halved flushCtx: a slow-but-
		// successful Flush must not eat into Close's deadline and turn an
		// otherwise-clean provider shutdown into a spurious ctx error on the Stop
		// result (OTEL-N5, K2).
		if err := metrics.Close(closeCtx); err != nil {
			errs = append(errs, err)
		}
	}

	// Close the tracer so buffered spans are flushed and the TracerProvider is
	// released on shutdown, bounded by closeCtx. NoopTracer.Close is a no-op, so
	// this is safe when tracing is disabled (K2).
	if tracer != nil {
		if err := tracer.Close(closeCtx); err != nil {
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
				rt.terminal = true
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
			if errors.Is(err, shared.ErrStaleFencingToken) {
				rt.mu.Unlock()
				return
			}
			rt.healthy = false
			rt.terminal = true
			cancel := rt.cancel
			rt.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	})
}

// superviseSession wraps a session manager's Run so a transient failure
// (broker reconnect, lease re-acquire) restarts JUST that session with jittered,
// capped exponential backoff, instead of tearing down the whole runtime and
// every unrelated route (C3-FU2). Isolation guarantees, by design:
//
//   - The global healthy/terminal flags are NEVER flipped, so a failing session
//     cannot trip /live (pod restart) or /health and sink unrelated routes.
//   - A permanent failure (bad broker URL, revoked credentials, a rejected
//     declare) is retried forever at the maxBackoff cap rather than escalated to
//     a pod restart — restarting the pod would kill every healthy co-tenant
//     session and, for a permanent fault, not fix anything. The fault stays
//     OBSERVABLE without a global signal: MetricSessionRestarts fires on every
//     restart (a continuous, rate-based, self-clearing flap signal), the session
//     is recorded in componentErrors (surfaced as failed_components), and the
//     session's live DeepHealth keeps readiness (/ready?level=...) red so the pod
//     is pulled from rotation. Alert on the MetricSessionRestarts rate or
//     readiness — not on liveness or a point-in-time failed_components snapshot,
//     which the pre-retry clear (below) can momentarily show empty even for a
//     still-failing session.
//   - Backoff resets to minBackoff once a run has stayed healthy for
//     stabilityWindow, so a fresh blip after hours of health retries promptly
//     (1s) instead of at the climbed 30s cap.
//   - componentErrors is cleared on a clean stop and before each retry, so a
//     recovered session does not leave a permanent phantom in failed_components;
//     a still-broken session re-records on its next failure.
//   - Equal-jitter spreads N sessions recovering from a shared broker/lease-store
//     outage so they do not reconnect in lockstep (thundering herd).
//
// A ErrStaleFencingToken is a clean stop (another instance owns the lease), not a
// fault to retry. A PANIC is intentionally NOT recovered here so it still
// propagates to startBackground's recover and remains terminal — a panic is a
// bug, fail-fast; only clean errors are isolated.
func (rt *Runtime) superviseSession(sid string, run func(context.Context) error) func(context.Context) error {
	const (
		minBackoff = 1 * time.Second
		maxBackoff = 30 * time.Second
		// stabilityWindow is how long a run must stay up before it counts as
		// "recovered": a run that survived a full cap interval has clearly
		// reconnected, so the climbed backoff ladder is reset.
		stabilityWindow = maxBackoff
	)
	name := "session:" + sid
	// rt.metrics is nil unless WithMetrics was supplied — the production builder
	// omits it and Start falls back to a Noop locally. Mirror that fallback: a
	// nil-deref on the restart counter would panic into startBackground's
	// recover and set terminal, i.e. the exact whole-runtime teardown this
	// isolation exists to prevent.
	metrics := rt.metrics
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	randFloat := rt.randFloat
	if randFloat == nil {
		randFloat = rand.Float64
	}
	return func(ctx context.Context) error {
		backoff := minBackoff
		for {
			runStart := rt.clk.Now()
			err := run(ctx)
			if err == nil || ctx.Err() != nil {
				rt.clearComponentError(name) // clean stop: drop any prior fault
				return nil
			}
			rt.setComponentError(name, err)
			if errors.Is(err, shared.ErrStaleFencingToken) {
				// Another instance won the lease; stop cleanly (unchanged
				// pre-existing semantics), leaving the recorded reason.
				return nil
			}
			metrics.Counter(shared.MetricSessionRestarts, 1,
				shared.Tag{Key: shared.TagKeySessionID, Value: sid})
			// A run that stayed healthy for a sustained window before failing
			// has recovered; forget the climbed ladder so this fresh blip
			// retries promptly instead of at the 30s cap.
			if rt.clk.Since(runStart) >= stabilityWindow {
				backoff = minBackoff
			}
			wait := equalJitter(backoff, randFloat)
			if rt.logger != nil {
				rt.logger.Warn("session component failed; restarting in isolation",
					"component", name, "error", err, "backoff", wait)
			}
			timer := rt.clk.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C():
			}
			// About to retry: drop the recorded fault so a run that now recovers
			// and stays healthy leaves no permanent phantom in componentErrors /
			// failed_components. A still-broken session re-records on its next
			// failure; MetricSessionRestarts is the authoritative flap signal.
			rt.clearComponentError(name)
			if backoff < maxBackoff {
				backoff = min(backoff*2, maxBackoff)
			}
		}
	}
}

// equalJitter applies equal-jitter to a restart backoff: the wait is half the
// backoff plus a random span over the other half, so wait ∈ [backoff/2, backoff).
// This spreads N session supervisors recovering from a shared broker or
// lease-store outage instead of letting them retry on identical wall-clock
// boundaries (thundering herd). randFloat returns a value in [0,1).
func equalJitter(backoff time.Duration, randFloat func() float64) time.Duration {
	half := backoff / 2
	return half + time.Duration(randFloat()*float64(half))
}

// setComponentError records a background component's failure for ComponentErrors
// (surfaced as failed_components in the health body).
func (rt *Runtime) setComponentError(name string, err error) {
	rt.mu.Lock()
	rt.componentErrors[name] = err
	rt.mu.Unlock()
}

// clearComponentError removes a previously-recorded component failure once the
// component has recovered or stopped cleanly, so failed_components does not
// report a stale phantom fault for the pod's remaining life.
func (rt *Runtime) clearComponentError(name string) {
	rt.mu.Lock()
	delete(rt.componentErrors, name)
	rt.mu.Unlock()
}

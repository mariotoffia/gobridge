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
	instanceID               string
	leaseOwnerID             string
	clk                      clock.Clock
	leaseStore               ports.LeaseStore
	outboxStore              ports.OutboxStore
	dlqStore                 ports.DLQStore
	managedSubscriptionStore ports.ManagedSubscriptionStore
	metrics                  ports.MetricsExporter
	audit                    ports.AuditLogger
	tracer                   ports.Tracer
	hook                     ports.DeliveryHook
	logger                   *slog.Logger
	globalMaxInFlight        int
	clusterEndpoints         map[string]string
	locator                  *cluster.Locator

	shutdownTimeout time.Duration

	// stopQuiesce OVERRIDES the pre-cancel drain budget in Stop. Stop always
	// drains in-flight deliveries BEFORE it cancels the runtime context (finding:
	// SIGTERM cancels work before quiescing): it waits for every route to reach
	// InFlight==0 (via WaitQuiescent) so a rolling restart lets current work
	// settle instead of aborting mid-delivery and forcing a duplicate on
	// redelivery. This field only tunes the budget; zero selects
	// defaultStopDrainBudget (see stopDrainBudget), NOT the old abort-style Stop.
	stopQuiesce time.Duration

	// randFloat supplies the [0,1) value used to jitter session-restart
	// backoff (superviseSession). Production leaves it nil and the supervisor
	// falls back to math/rand/v2.Float64; tests inject a deterministic source
	// so the jittered wait is reproducible under the fake clock.
	randFloat func() float64

	credRefresherClose func(context.Context)

	mu             sync.Mutex
	entries        []*routeEntry
	sessionSenders map[string]*sessionSenderEntry
	sessionMgrs    map[string]*session.Manager
	drainers       []*outbox.Drainer
	globalSem      chan struct{}
	running        bool
	healthy        bool
	terminal       bool
	// stopped records a clean, DELIBERATE Stop (an admin pause or a
	// supervisor swap of the old runtime). Unlike terminal it is NOT an
	// unrecoverable death: /live stays 200 and the liveness backstop must not
	// restart the process. It still makes the runtime single-use (Start
	// rejects), so "resume" means the supervisor builds a fresh runtime
	// (admin stop, healthy-swap liveness).
	stopped bool
	// stopDone is published by the Stop call that transitions the runtime to
	// terminal, and closed as the very last action of that Stop (after every
	// resource is released and errs is joined). A concurrent second Stop that
	// observes terminal blocks on it, so "Stop returned ⇒ resources released"
	// holds for every caller, not just the first (finding: double-Stop early
	// return). nil when terminal was tripped by a background component rather
	// than a Stop call.
	stopDone        chan struct{}
	componentErrors map[string]error
	// routeFlaps counts CONSECUTIVE sub-stability-window restarts per supervised
	// route (keyed "route:<id>", like componentErrors). superviseRoute increments
	// it on a quick flap and resets it on a stable run; DeepHealth latches
	// RouteHealth.RouteDead once it reaches routeDeadRestartThreshold.
	routeFlaps map[string]int
	// routeRunStart records the wall-clock start of each supervised route's
	// CURRENT run (keyed "route:<id>"), set before run() and cleared after it
	// returns. DeepHealth reads it to suppress a latched route_dead once the live
	// run has outlived the stability window: a route that flaps to the threshold
	// then recovers and keeps running never re-enters superviseRoute to reset the
	// flap counter, so without this it would advertise route_dead forever.
	routeRunStart map[string]time.Time
	cancel        context.CancelFunc
	wg            sync.WaitGroup
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

// WithStopQuiesce OVERRIDES the pre-cancel drain budget used by Stop. Stop now
// ALWAYS settles in-flight deliveries before cancelling (see stopDrainBudget /
// defaultStopDrainBudget); this option only tunes how long that settle phase may
// take. When budget > 0, Stop waits up to budget for all routes to reach
// InFlight==0 BEFORE cancelling the runtime context, so in-flight deliveries
// settle rather than being aborted mid-flight (which would resurface them as
// duplicates on redelivery). Passing 0 selects the default ceiling, not the old
// abort-style Stop.
//
// NOTE: a rolling restart should already have removed this instance from ingress
// (e.g. pod marked NotReady) before Stop; the quiesce then drains the residual
// in-flight set. Under continuous ingress the drain relies on the budget cap
// (an independent receiver-pause lives in the route stream, out of this scope).
func WithStopQuiesce(budget time.Duration) Option {
	return func(rt *Runtime) { rt.stopQuiesce = budget }
}

// WithOutboxStore sets the durable outbox store for SharedOutbox delivery.
func WithOutboxStore(store ports.OutboxStore) Option {
	return func(rt *Runtime) { rt.outboxStore = store }
}

// WithDLQStore sets the dead-letter queue store.
func WithDLQStore(store ports.DLQStore) Option {
	return func(rt *Runtime) { rt.dlqStore = store }
}

// WithManagedSubscriptionStore gives the runtime lifecycle ownership of the
// connectivity history store; sessions use the same handle through SessionSpec.
func WithManagedSubscriptionStore(store ports.ManagedSubscriptionStore) Option {
	return func(rt *Runtime) { rt.managedSubscriptionStore = store }
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
	// The lease ownerID carries a per-process boot nonce so two replicas that
	// share the SAME instance_id (e.g. a mis-set env var, a cloned deployment)
	// derive DISTINCT lease owners. Without it their identical ownerID matched
	// the lease store's same-owner fast path and each replica instantly
	// re-seized the other's lease — a permanent ping-pong that reset every
	// standby's observation window and starved failover.
	// instance_id remains the human-facing display identity (logs, health, the
	// source-id header); only the lease ownership token is nonce-suffixed.
	rt.leaseOwnerID = rt.instanceID + "#" + generateID()
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
// waits for them to finish, then closes sessions, stores, and telemetry.
// If ctx expires before goroutines finish, teardown still proceeds under a
// bounded detached context and an error is returned.
//
// Stop is idempotent and safe to call on a runtime that was BUILT but never
// Started: the composition-root supervisor calls Stop() on a runtime whose
// swap failed, so Stop must release every prep-opened resource — lease/outbox/
// DLQ stores, all opened sessions (including unmanaged binding sessions), and
// any session managers — even though no background goroutine ever ran. After
// Stop the runtime is terminal and cannot be restarted (finding 3).
func (rt *Runtime) Stop(ctx context.Context) error {
	rt.mu.Lock()
	if (rt.terminal || rt.stopped) && !rt.running {
		// A prior Stop already transitioned this runtime to stopped/terminal.
		if rt.stopDone != nil {
			// That Stop's teardown may still be in flight. Block until it has
			// fully completed so a caller relying on "Stop returned ⇒ resources
			// released" does not proceed early (e.g. reopen a store whose handle
			// the first Stop is about to close) — finding: double-Stop early
			// return.
			stopDone := rt.stopDone
			rt.mu.Unlock()
			select {
			case <-stopDone:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// No stopDone recorded: terminal was tripped without a Stop-driven
		// teardown ever starting. Idempotent no-op.
		rt.mu.Unlock()
		return nil
	}
	// This call performs the teardown (either the first Stop, or a Stop after a
	// background component tripped terminal but left running=true). Publish a
	// stopDone that any concurrent second Stop blocks on until we are done.
	stopDone := make(chan struct{})
	rt.stopDone = stopDone
	rt.running = false
	// A deliberate Stop is a clean pause, NOT an unrecoverable death: mark the
	// runtime stopped (single-use) but do NOT trip terminal. terminal keeps
	// whatever a background component already set (bridge.go component-failure
	// trips), so an ABNORMAL stop stays terminal and the liveness backstop still
	// restarts the process, while a clean admin/swap Stop leaves /live at 200.
	rt.stopped = true
	cancel := rt.cancel
	rt.mu.Unlock()

	// close(stopDone) MUST be the very last thing Stop does. Registered first
	// ⇒ runs last (LIFO), after closeCancel/flushCancel and after the return
	// value (errors.Join) is computed — so a blocked second Stop only observes
	// completion once every resource is released.
	defer close(stopDone)

	if logging.DebugEnabled(rt.logger) {
		rt.logger.Log(ctx, logging.LevelDebug, "runtime stopping",
			"instance_id", rt.instanceID,
		)
	}

	// cancel is nil for a built-but-never-started runtime (Start assigns it).
	if cancel != nil {
		// Drain state machine (ports.RuntimeCommand.Stop). Only `running` was
		// flipped false above (`healthy` is intentionally left true — a clean
		// Stop is a pause, not a failure), so readiness (running && healthy) has
		// gone false. Readiness=false only sheds PUSH traffic: an upstream LB or
		// service mesh stops routing new requests to this instance. PULL
		// transports (SQS/Kafka receivers) are NOT gated by readiness and keep
		// pulling from the broker until `cancel()` below tears their loop down —
		// so there is a residual intake/redelivery window between readiness going
		// false and cancel firing. Now SETTLE already-accepted in-flight
		// deliveries BEFORE cancelling: aborting mid-send/mid-ack turns
		// every rolling restart into a duplicate/loss window (a canceled context
		// fails the source ack, so the broker redelivers an already-sent message),
		// whereas letting accepted work finish its send+settle first avoids it.
		//
		// This runs by DEFAULT now, not only under WithStopQuiesce (finding: SIGTERM
		// cancels work before quiescing). The wait is bounded — deadline fallback:
		// when the budget or the caller ctx expires we cancel remaining work and
		// return, leaving any unsettled source to broker redelivery (at-least-once),
		// never silently acked. WaitQuiescent acquires rt.mu, which we released above.
		if budget := rt.stopDrainBudget(); budget > 0 && ctx.Err() == nil && rt.anyRouteInFlight() {
			qCtx, qCancel := context.WithTimeout(ctx, budget)
			if err := rt.WaitQuiescent(qCtx, QuiescenceOptions{}); err != nil && rt.logger != nil {
				rt.logger.Warn("stop drain did not fully settle in-flight deliveries before deadline; cancelling (unsettled sources rely on broker redelivery)",
					"instance_id", rt.instanceID, "budget", budget, "error", err)
			}
			qCancel()
		}
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
	// only until rt.wg drains. For a never-started runtime rt.wg is empty and
	// this returns immediately. If ctx fires first we record the error and
	// proceed to teardown under the bounded closeCtx (below).
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		rt.wg.Wait()
	}()

	drainersDone := false
	select {
	case <-waitDone:
		drainersDone = true
	case <-ctx.Done():
		// Pre-cancelled / early-expiring ctx: we stop waiting for background
		// goroutines and proceed to close sessions/telemetry below. A straggler
		// (e.g. a drainer still finalising) may therefore emit a counter or span
		// after its provider is closed. That is benign: OTel Counter/Start calls
		// on a shut-down provider are no-ops and the SDK is concurrency-safe, so
		// no panic, race, or corruption results (OTEL).
		errs = append(errs, ctx.Err())
	}

	// Finding: manager-close gating. A drainer's finalDrain (outbox/loop.go)
	// runs under context.WithoutCancel plus a bounded grace, so it can still be
	// mid-send after the caller ctx expired above. Closing a session manager
	// (which releases the lease and closes the session) out from under such a
	// send makes the drainer's subsequent Complete fail with a stale fencing
	// token — the record resurfaces on restart as a duplicate send. So, exactly
	// as the store-close does below, wait a bounded grace for the drainer wg to
	// CONFIRM done BEFORE we close managers. The grace is
	// clampedStoreCloseGrace(ctx): the policy-derived worst-case, but never longer
	// than the incoming shutdown ctx's remaining deadline so the detached
	// (WithoutCancel) wait cannot outlive the platform kill budget and get
	// SIGKILLed mid-drain. If the grace elapses we still close managers (leases
	// MUST release so another instance can take over — a stale-token Complete from
	// a genuinely stuck drainer is the lesser evil), but in the common case
	// finalDrain's Complete runs against a live manager/lease.
	if !drainersDone {
		graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), rt.clampedStoreCloseGrace(ctx))
		select {
		case <-waitDone:
			drainersDone = true
		case <-graceCtx.Done():
		}
		graceCancel()
	}

	// Close/flush must complete regardless of caller ctx cancellation,
	// but we preserve values (trace/correlation) for logging.
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer closeCancel()

	// Snapshot the managers, sessions and stores under the lock, then RELEASE it
	// before invoking the (potentially blocking) plugin Close calls. A wedged
	// broker client's Close must not hold rt.mu — that would stall Role(),
	// DeepHealth and the /live+/ready probes for the whole Stop duration
	// (finding L-M).
	rt.mu.Lock()
	managed := make(map[string]bool, len(rt.sessionMgrs))
	mgrs := make([]*session.Manager, 0, len(rt.sessionMgrs))
	for sid, mgr := range rt.sessionMgrs {
		managed[sid] = true
		mgrs = append(mgrs, mgr)
	}
	type sessRef struct {
		sid  string
		sess ports.Session
	}
	sessRefs := make([]sessRef, 0, len(rt.entries)+len(rt.sessionSenders))
	for _, entry := range rt.entries {
		sid := ""
		if entry.sessCfg != nil {
			sid = entry.sessCfg.SessionID
		}
		sessRefs = append(sessRefs, sessRef{sid: sid, sess: entry.session})
	}
	for sid, sse := range rt.sessionSenders {
		sessRefs = append(sessRefs, sessRef{sid: sid, sess: sse.session})
	}
	metrics := rt.metrics
	stores := []any{rt.managedSubscriptionStore, rt.outboxStore, rt.dlqStore, rt.leaseStore}
	rt.mu.Unlock()

	for _, mgr := range mgrs {
		if err := mgr.Close(closeCtx); err != nil {
			errs = append(errs, err)
		}
	}
	// close every session that no manager owns. A built-but-never-started
	// runtime has no managers at all, so ALL of its opened sessions land here;
	// a started runtime reaches here only for unmanaged binding sessions
	// (session senders never promoted to a drainer). Dedupe by pointer so a
	// session shared across entries/binding-senders is closed exactly once.
	closedSessions := make(map[ports.Session]bool)
	for _, ref := range sessRefs {
		if ref.sess == nil || managed[ref.sid] || closedSessions[ref.sess] {
			continue
		}
		closedSessions[ref.sess] = true
		if err := ref.sess.Close(closeCtx); err != nil {
			errs = append(errs, err)
		}
	}

	// Finding 13: the durable stores must NOT be released while a drainer's
	// finalDrain is still mid-send. finalDrain runs under context.WithoutCancel
	// plus a bounded grace (outbox/loop.go), so it can outlive the caller's Stop
	// ctx by design. Closing the SQLite handle underneath an in-flight Complete
	// would, after a SUCCESSFUL send, drop the terminal state update and
	// resurface the record on restart as a duplicate. Only release store handles
	// once the drainer wg has CONFIRMED done. The bounded grace-wait above
	// (before manager close) already gave the drainers up to storeCloseGrace to
	// confirm; if they still have not (a genuinely stuck drainer) we skip the
	// close and leak the handle rather than risk mid-send corruption — a fresh
	// runtime opens its own handles on reload, so a leaked handle is the strictly
	// safer failure.
	//
	// Closing the managers/sessions above can take time; a drainer whose grace
	// expired at the earlier wait may have finished during that window. Re-check
	// non-blockingly here so we release the handles instead of leaking them when
	// the drainer has, in fact, completed.
	if !drainersDone {
		select {
		case <-waitDone:
			drainersDone = true
		default:
		}
	}
	if drainersDone {
		// Release store resources (e.g. SQLite file handles). Stores that hold
		// OS resources implement io.Closer; in-memory stores do not and are
		// skipped. Reconfiguration always builds a fresh runtime with its own
		// store instances before Stopping the old one, so a closed handle is
		// never shared with a live runtime. (Finding + finding 13.)
		for _, s := range stores {
			if c, ok := s.(io.Closer); ok {
				if err := c.Close(); err != nil {
					errs = append(errs, err)
				}
			}
		}
	} else {
		errs = append(errs, errors.New("runtime: stop: drainers did not confirm done before store-close grace; leaving store handles open to avoid mid-send corruption"))
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
		// Stop FLUSHES buffered data on every runtime stop but does NOT Close the
		// exporter: the metrics exporter and tracer are shared by every runtime
		// across config reloads and are owned by the composition root that
		// supplied them, which Closes them exactly once at process shutdown. A
		// per-runtime Close here killed the shared CloudWatch flush goroutine on
		// the FIRST reload, so all later metrics were silently dropped for the
		// process lifetime.
		//
		// The tracer (ports.Tracer) exposes only Close, no Flush, so there is
		// nothing to flush per-stop; like the metrics exporter it is neither
		// flushed nor Closed here. Its owner — the composition root that passed it
		// via bridge.WithSupervisorTracer — Closes it exactly once at process
		// shutdown. (No in-tree composition root wires a real tracer today; the
		// cmd/gobridge example shows the owner-Closes-once pattern.)
	}

	return errors.Join(errs...)
}

// storeCloseGrace is the FLOOR that Stop waits for the drainer waitgroup to
// confirm done before (a) closing session managers and (b) releasing durable
// store handles, when the caller ctx has already expired (finding 13). It is a
// floor, not the whole budget: effectiveStoreCloseGrace raises it to cover the
// configured policies' worst-case single in-flight completion so a legitimate
// final drainer send is never cut off mid-flight (see effectiveStoreCloseGrace).
const storeCloseGrace = 15 * time.Second

// completeBudgetCeiling mirrors the drainer's completeCtx/completeBudget clamp
// (outbox/retry.go): the post-send Complete/Release window is bounded to at most
// 5s (min(SendTimeout, 5s) for a positive SendTimeout). Kept here as a named
// ceiling so effectiveStoreCloseGrace derives the SAME worst-case the drainer
// actually uses without a ports/outbox change.
const completeBudgetCeiling = 5 * time.Second

// effectiveStoreCloseGrace returns the grace Stop must wait for the drainers to
// confirm done, coherent with the drainers' worst-case single in-flight send.
//
// The inherited hazard (outbox/retry.go:131-141): a drainer's finalDrain runs
// under context.WithoutCancel and can legitimately spend up to SendTimeout on the
// final send plus up to completeBudget() (== min(SendTimeout, 5s)) on the
// post-send Complete. If the bare 15s storeCloseGrace elapses first, Stop closes
// the session manager — which clears the lease — so the drainer's runtime-side
// post-send lease fence refuses the final Complete and the record resurfaces on
// restart as an AVOIDABLE duplicate. To stay coherent, the grace must be at least
// the largest such worst-case across every shared-outbox route policy:
//
//	worst(entry) = SendTimeout + min(SendTimeout, 5s)   // after WithDefaults
//	grace        = max(storeCloseGrace floor, max_entries worst(entry))
//
// The floor (15s) still applies when no policy demands more (e.g. no routes, or
// tiny SendTimeouts). Single-owner failover semantics are preserved: the grace is
// still a BOUNDED wait — once it elapses Stop closes managers and releases the
// lease regardless (the "lesser evil" of a stale-token Complete from a genuinely
// stuck drainer, per the wait sites' comments), so leases always eventually
// release for a standby to take over.
func (rt *Runtime) effectiveStoreCloseGrace() time.Duration {
	grace := storeCloseGrace
	rt.mu.Lock()
	entries := rt.entries
	rt.mu.Unlock()
	for i := range entries {
		p := entries[i].config.Policy.WithDefaults()
		st := p.SendTimeout
		if st <= 0 {
			continue
		}
		complete := st
		if complete > completeBudgetCeiling {
			complete = completeBudgetCeiling
		}
		if worst := st + complete; worst > grace {
			grace = worst
		}
	}
	return grace
}

// storeCloseGraceMargin is subtracted from the incoming shutdown ctx's remaining
// deadline when clamping the store-close grace, so the bounded manager-close wait
// leaves a little headroom for the caller to observe the clamp rather than
// consuming the ENTIRE remaining budget right up to the platform kill instant.
const storeCloseGraceMargin = 1 * time.Second

// clampedStoreCloseGrace bounds the derived store-close grace by the incoming
// shutdown ctx's remaining deadline.
//
// effectiveStoreCloseGrace can derive a grace as large as
// SendTimeout + min(SendTimeout, 5s) (~65s for a 60s SendTimeout), and the
// grace-wait DETACHES from ctx via context.WithoutCancel so the drain survives
// caller cancellation. Detaching an UNCLAMPED grace lets that wait outlive the
// platform's OWN kill budget — ECS StopTimeout / K8s terminationGracePeriod,
// default 60s — so the process is SIGKILLed mid-drain: the exact avoidable
// duplicate + lost in-flight the coherence raise was meant to PREVENT (raising
// the bare 15s floor is what created this exposure). So when the caller ctx
// carries a deadline, clamp the grace to the remaining time (minus a small margin
// for the close phase that follows); with no deadline the derived grace stands (a
// deadline-less Background caller cannot be SIGKILLed by a platform budget this
// layer can observe). The value-detachment (trace/correlation) at the wait site
// is unaffected — only the WAIT duration is bounded, never below zero.
func (rt *Runtime) clampedStoreCloseGrace(ctx context.Context) time.Duration {
	grace := rt.effectiveStoreCloseGrace()
	deadline, ok := ctx.Deadline()
	if !ok {
		return grace
	}
	// Compute the remaining budget via the injected clock (never time.Until):
	// rt.clk is clock.System (real wall-clock) in production — matching both the
	// caller's real-time ctx deadline and the WithTimeout timer below — and is a
	// fake clock only under test.
	if remaining := deadline.Sub(rt.clk.Now()) - storeCloseGraceMargin; remaining < grace {
		grace = remaining
	}
	if grace < 0 {
		grace = 0
	}
	return grace
}

// defaultStopDrainBudget caps the pre-cancel in-flight settle phase of Stop when
// no explicit WithStopQuiesce budget was set. It is the ceiling that keeps a Stop
// with a deadline-less caller ctx (e.g. context.Background()) from blocking
// forever behind a wedged sender: at worst Stop settles for this long, then falls
// through to cancel + broker redelivery (deadline fallback). A caller ctx with a
// shorter deadline still wins — the drain honours whichever fires first.
const defaultStopDrainBudget = 25 * time.Second

// stopDrainBudget returns the bounded budget for Stop's pre-cancel in-flight
// settle phase. An explicit WithStopQuiesce wins; otherwise the default ceiling
// applies. The caller ctx additionally bounds the wait (see the WithTimeout(ctx,
// budget) at the call site), so a short SIGTERM grace period is respected without
// this method having to inspect the deadline.
func (rt *Runtime) stopDrainBudget() time.Duration {
	if rt.stopQuiesce > 0 {
		return rt.stopQuiesce
	}
	return defaultStopDrainBudget
}

// anyRouteInFlight reports whether any route runner currently has an in-flight
// delivery. Stop uses it to SKIP the pre-cancel drain when the runtime is already
// quiescent — the common case — so an idle Stop cancels promptly instead of
// waiting out a settle/quiet window. A delivery accepted in the narrow window
// between this snapshot and cancel is the same at-least-once boundary the
// deadline fallback already documents (broker redelivery, never a silent ack).
func (rt *Runtime) anyRouteInFlight() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, e := range rt.entries {
		if e.runner != nil && e.runner.InFlight() > 0 {
			return true
		}
	}
	return false
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
				// A stale fencing token means THIS instance lost its exclusive
				// lease to another owner: the component stopped deliberately, so it
				// must NOT flip global health or cancel the runtime — a demoted
				// standby stays live to resume on re-acquire. This is safe ONLY
				// because every component that can legitimately lose a fence (the
				// session managers) runs under superviseSession, which owns the
				// lease lifecycle and escalates a terminal loss ITSELF; the bare
				// startBackground path is never wired to a fencing-capable
				// component today.
				//
				// HAZARD: if a future component IS wired through bare
				// startBackground and returns ErrStaleFencingToken, its goroutine
				// would stop here with rt.healthy still true — a silent,
				// unsupervised death behind a green liveness probe. Log loudly so
				// that wiring mistake is visible rather than mute; do not remove
				// this branch on the assumption it is unreachable.
				if rt.logger != nil {
					rt.logger.Error("background component stopped on stale fencing token without supervision; "+
						"global health left unchanged (expected only for lease-supervised components — "+
						"a bare startBackground component reaching here dies silently)",
						"component", name, "error", err)
				}
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

// errSessionUnexpectedStop marks a session manager's Run returning nil while the
// runtime is still live — an anomalous early exit (e.g. a closed broker Events
// channel mis-reported as a clean stop). superviseSession converts a live-ctx
// nil return into this error so the death is surfaced as a per-session
// degradation and restarted in isolation, never swallowed as a clean stop
// (finding 23).
var errSessionUnexpectedStop = errors.New("runtime: session manager stopped unexpectedly while runtime is running")

// superviseSession wraps a session manager's Run so a transient failure
// (broker reconnect, lease re-acquire) restarts JUST that session with jittered,
// capped exponential backoff, instead of tearing down the whole runtime and
// every unrelated route. Isolation guarantees, by design:
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
// A ErrStaleFencingToken (another instance currently owns the lease) is treated
// as RESTARTABLE, not a clean stop: the manager is re-run so a standby keeps
// supervising and can re-acquire on the next transfer, instead of silently
// abandoning failover duty. A ErrSessionUnrecoverable (a single-use
// session that can no longer Start after a step-down Close) is ESCALATED to
// terminal — the manager has already released the lease, so a standby takes over
// while the orchestrator restarts this pod with a fresh session instance; looping
// on the dead instance would re-seize the lease via the store's same-owner fast
// path and wedge the cluster. A PANIC is intentionally NOT
// recovered here so it still propagates to startBackground's recover and remains
// terminal — a panic is a bug, fail-fast; only transient errors are isolated and
// retried.
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
			if ctx.Err() != nil {
				// Runtime is shutting down: a nil (or any) return is a genuine
				// clean stop. Drop any prior fault and exit.
				rt.clearComponentError(name)
				return nil
			}
			if err == nil {
				// Finding 23: the session manager returned nil while the runtime
				// is still live. A session should only stop cleanly on shutdown
				// (ctx cancelled, handled above) or on ErrStaleFencingToken
				// (lease lost, handled below). A nil return here means the
				// session's event loop ended unexpectedly — e.g. a closed broker
				// Events channel that the manager mis-reports as a clean stop.
				// Exiting silently would strand a dead session while the runtime
				// still advertises it healthy and ready. Surface it as a
				// per-session degradation (failed_components + MetricSessionRestarts)
				// and restart it in isolation like any other transient fault,
				// rather than tearing down the whole runtime.
				err = errSessionUnexpectedStop
			}
			rt.setComponentError(name, err)
			// A ErrStaleFencingToken means another instance currently owns the
			// lease. This is NOT a reason to abandon the session supervisor: a
			// standby that stops supervising can never re-acquire when the active
			// instance later steps down, silently removing the only failover
			// target. It falls through to the jittered capped
			// backoff below and re-runs the manager, which re-enters its
			// acquire/standby loop — keeping standby duty alive.
			if errors.Is(err, session.ErrSessionUnrecoverable) {
				// A single-use session was closed by a prior step-down and
				// cannot be re-Started in this process. Retrying the dead
				// instance forever is the zombie loop: each retry
				// re-Acquired the lease via the store's same-owner fast path,
				// bumped the version and reset every standby's observation
				// window, wedging the whole cluster while liveness stayed green.
				// Escalate to terminal instead: the manager already RELEASED the
				// lease (a healthy standby takes over immediately) and returning
				// the error flips startBackground terminal so the orchestrator
				// restarts this pod with a fresh session instance (documented
				// process-restart backstop, scenario-08).
				return err
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

// errRouteUnexpectedStop marks a route runner's Run returning nil while the
// runtime is still live — the receiver's receive loop ended without a shutdown
// signal (e.g. a broker stream closed and the adapter returned nil instead of an
// error). superviseRoute converts a live-ctx nil return into this error so the
// stop is surfaced as a per-route degradation and the route restarts in
// isolation, rather than being silently stranded while the runtime keeps
// advertising it ready.
var errRouteUnexpectedStop = errors.New("runtime: route runner stopped unexpectedly while runtime is running")

// superviseRoute wraps a route runner's Run so a failing route is isolated with
// jittered, capped exponential backoff instead of tearing down the whole runtime
// (findings "one permanent link error kills every route" and "one
// permanently-failing route tears down the entire runtime"). It MIRRORS
// superviseSession's isolation contract:
//
//   - The global healthy/terminal flags are NEVER flipped, so one route with a
//     deleted queue or a revoked permission cannot trip /live (a whole-pod
//     CrashLoopBackOff) or sink every healthy co-tenant route sharing the pod.
//   - A genuinely-permanent route fault stays OBSERVABLE without a global signal:
//     MetricRouteRestarts fires on every restart (a continuous, rate-based flap
//     signal, tagged by route_id), the route is recorded in componentErrors
//     (surfaced as failed_components), and DeepHealth marks that route not-ready
//     (bridge_health.go ANDs the recorded fault into RouteHealth.Ready) so a
//     level=full readiness probe pulls the pod from rotation for that route while
//     healthy routes keep serving.
//   - Backoff resets to minBackoff after a run stays healthy for stabilityWindow;
//     componentErrors is cleared before each retry so a recovered route leaves no
//     phantom fault; equal-jitter avoids lockstep reconnection.
//
// This REPLACES the previous fail-fast rationale (the old REV-3-routeiso comment
// at the route start site). That argument held that every non-ctx error a
// receiver surfaces is unrecoverable-and-global and SHOULD crash the pod, on the
// premise that adapters isolate all transient faults internally. Weighed
// honestly, its fatal flaw is co-tenancy: a runtime hosts MANY routes, and a
// fault that is permanent for ONE route (its queue deleted, its credential
// revoked, a protocol mismatch on that link) is NOT global — crashing the pod
// punishes every healthy route and, because the fault is permanent, the restart
// just CrashLoopBackOffs without fixing anything. Per-route isolation keeps the
// blast radius at the faulty route while preserving full observability, which is
// strictly better for a multi-route pod. The honest tradeoff: a single-use
// receiver whose Run cannot be re-entered (its Close already ran) will fail fast
// on restart and settle at the backoff cap rather than reconnect — that is the
// same capped-flap behaviour superviseSession already accepts, and it stays
// visible via MetricRouteRestarts + readiness rather than being masked.
func (rt *Runtime) superviseRoute(routeID string, run func(context.Context) error) func(context.Context) error {
	const (
		minBackoff      = 1 * time.Second
		maxBackoff      = 30 * time.Second
		stabilityWindow = routeStabilityWindow
	)
	name := "route:" + routeID
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
			rt.setRouteRunStart(name, runStart)
			err := run(ctx)
			rt.clearRouteRunStart(name)
			if ctx.Err() != nil {
				// Runtime shutting down: a clean stop. Drop any prior fault.
				rt.clearComponentError(name)
				rt.resetRouteFlap(name)
				return nil
			}
			if err == nil {
				// Receiver loop ended without a shutdown signal — surface it as a
				// per-route degradation and restart in isolation rather than
				// stranding a dead route while the runtime advertises it ready.
				err = errRouteUnexpectedStop
			}
			rt.setComponentError(name, err)
			if errors.Is(err, route.ErrRouteTerminal) {
				// the route runner declared itself
				// UNRESTARTABLE in this process — either its single-use receiver
				// was already Closed and cannot be re-Run (a fresh Run would flap
				// the SAME dead instance at the backoff cap forever behind green
				// liveness, since AddRoute stores built receiver/session/sender
				// INSTANCES, not factories, so the supervisor has nothing to
				// rebuild from), or the runner wedged after a hung send / a
				// timeout-abandoned processor storm and must not keep accepting
				// work. Silent capped flapping is exactly the "permanently dead
				// behind green process liveness with no actionable signal" hazard
				// the finding calls out. Escalate to terminal instead (mirrors
				// superviseSession's ErrSessionUnrecoverable branch): the error
				// flips startBackground terminal so the orchestrator restarts this
				// pod with freshly-built transports (documented process-restart
				// backstop). The route is already recorded in componentErrors and
				// MetricRouteRestarts fired, so the escalation stays observable.
				metrics.Counter(shared.MetricRouteRestarts, 1,
					shared.Tag{Key: shared.TagKeyRouteID, Value: routeID})
				return err
			}
			metrics.Counter(shared.MetricRouteRestarts, 1,
				shared.Tag{Key: shared.TagKeyRouteID, Value: routeID})
			if rt.clk.Since(runStart) >= stabilityWindow {
				// The run reached the stability window before failing: treat it as a
				// fresh healthy-then-degraded cycle — reset both the backoff and the
				// consecutive-flap counter that drives route_dead.
				backoff = minBackoff
				rt.resetRouteFlap(name)
			} else {
				// A sub-stability-window failure — another quick flap. After
				// routeDeadRestartThreshold of these in a row with no stable run
				// between, DeepHealth latches route_dead=true so ops alert on the
				// steady STATE (a route wedged at the backoff cap behind a green
				// liveness probe) rather than on the restart RATE alone.
				rt.recordRouteFlap(name)
			}
			wait := equalJitter(backoff, randFloat)
			if rt.logger != nil {
				rt.logger.Warn("route component failed; restarting in isolation",
					"component", name, "error", err, "backoff", wait)
			}
			timer := rt.clk.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C():
			}
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

// routeStabilityWindow is how long a supervised route's CURRENT run must stay up
// before it counts as recovered. Two places must agree on it: superviseRoute
// resets the flap counter when a run RETURNS after this window, and the DeepHealth
// route_dead projection suppresses a latched dead state when the LIVE run has
// already outlived it (a recovered route that keeps running never re-enters
// superviseRoute to reset). Equal to the supervisor backoff cap (30s): a run that
// outlives one full backoff has cleared the flap regime.
const routeStabilityWindow = 30 * time.Second

// routeDeadRestartThreshold is the number of CONSECUTIVE sub-stability-window
// route restarts (quick flaps with no stable run between them) after which
// DeepHealth latches RouteHealth.RouteDead=true. A route that flaps this
// many times has almost certainly wedged at the supervisor backoff cap — e.g. a
// single-use receiver whose Run cannot be re-entered — so ops can alert on that
// steady STATE rather than on the restart rate. Kept small: with the
// 1→2→4→8→16s fast-backoff ramp it is ~31s of continuous flapping before the
// signal latches.
const routeDeadRestartThreshold = 5

// recordRouteFlap increments a route's consecutive sub-stability-window restart
// counter. Called by superviseRoute when a run fails before reaching the
// stability window. Lazily allocates the map so it is safe even when a test
// drives superviseRoute directly, bypassing Start's allocation.
func (rt *Runtime) recordRouteFlap(name string) {
	rt.mu.Lock()
	if rt.routeFlaps == nil {
		rt.routeFlaps = make(map[string]int)
	}
	rt.routeFlaps[name]++
	rt.mu.Unlock()
}

// resetRouteFlap clears a route's consecutive-flap counter once a run stayed up
// for the stability window (a recovery) or the route stopped cleanly, so a
// recovered route drops route_dead instead of latching it forever. This
// fires only when a run RETURNS after the window; a route that recovers and keeps
// running is handled complementarily by the DeepHealth read-time liveness check
// (see routeRunStart). delete on a nil map is a no-op, so this is safe even
// before Start allocates the map.
func (rt *Runtime) resetRouteFlap(name string) {
	rt.mu.Lock()
	delete(rt.routeFlaps, name)
	rt.mu.Unlock()
}

// setRouteRunStart records the start time of a supervised route's current run so
// DeepHealth can distinguish a live, recovered route (its run has outlived the
// stability window) from one still wedged in the flap regime. Lazily
// allocates the map like recordRouteFlap so tests driving superviseRoute directly
// are safe.
func (rt *Runtime) setRouteRunStart(name string, at time.Time) {
	rt.mu.Lock()
	if rt.routeRunStart == nil {
		rt.routeRunStart = make(map[string]time.Time)
	}
	rt.routeRunStart[name] = at
	rt.mu.Unlock()
}

// clearRouteRunStart drops the current-run marker when a run returns (the route is
// now between runs — in backoff or being re-entered), so route_dead is not
// suppressed while the route is not actually up. delete on a nil map is a
// no-op.
func (rt *Runtime) clearRouteRunStart(name string) {
	rt.mu.Lock()
	delete(rt.routeRunStart, name)
	rt.mu.Unlock()
}

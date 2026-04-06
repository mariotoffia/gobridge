package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	goruntime "runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Runtime is the top-level coordinator for the GoBridge message routing
// engine. It manages routes, sessions, and outbox drainers.
// Start returns immediately after spawning background goroutines.
// Stop cancels the context, waits for all goroutines to finish, then
// closes sessions.
type Runtime struct {
	instanceID        string
	leaseStore        ports.LeaseStore
	outboxStore       ports.OutboxStore
	dlqStore          ports.DLQStore
	metrics           ports.MetricsExporter
	audit             ports.AuditLogger
	tracer            ports.Tracer
	logger            *slog.Logger
	globalMaxInFlight  int
	clusterEndpoints   map[string]string
	locator            *routeLocator

	mu              sync.Mutex
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
	}
	for _, opt := range opts {
		opt(rt)
	}
	return rt
}

// RegisterSessionSender registers a session and its sender for use as
// an egress target. This is needed when SharedOutbox routes fan out to
// sessions that are not the route's primary session (e.g. one SQS source
// writes to outbox partitions for multiple exclusive MQTT clients).
// The runtime creates a SessionManager and OutboxDrainer for each
// registered session during Start.
func (rt *Runtime) RegisterSessionSender(
	cfg SessionConfig,
	session ports.Session,
	sender ports.Sender,
) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.running {
		return errors.New("cannot register session sender on running runtime")
	}
	if cfg.SessionID == "" {
		return errors.New("session ID is required")
	}

	if _, exists := rt.sessionSenders[cfg.SessionID]; exists {
		return errors.New("duplicate session sender: " + cfg.SessionID)
	}

	rt.sessionSenders[cfg.SessionID] = &sessionSenderEntry{
		config:  cfg,
		session: session,
		sender:  sender,
	}
	return nil
}

// AddRoute registers a route with its transport instances and optional
// session configuration. The route is not started until Start is called.
func (rt *Runtime) AddRoute(
	cfg RouteConfig,
	receiver ports.Receiver,
	sender ports.Sender,
	session ports.Session,
	sessCfg *SessionConfig,
) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.running {
		return errors.New("cannot add route to running runtime")
	}

	for _, e := range rt.entries {
		if e.config.ID == cfg.ID {
			return errors.New("duplicate route ID: " + cfg.ID)
		}
	}

	rt.entries = append(rt.entries, &routeEntry{
		config:   cfg,
		receiver: receiver,
		sender:   sender,
		session:  session,
		sessCfg:  sessCfg,
	})
	return nil
}

// Start wires up all registered routes, session managers, and outbox
// drainers, then spawns background goroutines. It returns immediately;
// use Stop to shut down gracefully.
func (rt *Runtime) Start(ctx context.Context) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.running {
		return errors.New("runtime already running")
	}
	rt.running = true
	rt.healthy = true
	rt.componentErrors = make(map[string]error)

	if err := validateRoutes(rt.entries, rt.outboxStore != nil, rt.leaseStore != nil, rt.dlqStore != nil); err != nil {
		rt.running = false
		return err
	}

	logging.Debug(rt.logger, "runtime starting",
		"instance_id", rt.instanceID,
		"route_count", len(rt.entries),
		"session_count", len(rt.sessionMgrs)+len(rt.sessionSenders),
	)

	ctx, rt.cancel = context.WithCancel(ctx)

	dlq := NewDLQRouter(rt.dlqStore)
	dlq.Start(ctx)
	rt.dlqRouter = dlq

	m := rt.metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}

	if rt.globalMaxInFlight > 0 {
		rt.globalSem = make(chan struct{}, rt.globalMaxInFlight)
	}

	if rt.leaseStore != nil {
		rt.locator = newRouteLocator(rt.instanceID, rt.leaseStore)
	}

	drainerSessions := make(map[string]bool)

	for _, entry := range rt.entries {
		entry.runner = newRouteRunner(RouteRunnerConfig{
			RouteID:       entry.config.ID,
			Policy:        entry.config.Policy,
			Receiver:      entry.receiver,
			Sender:        entry.sender,
			Senders:       entry.config.Senders,
			OutboxStore:   rt.outboxStore,
			DLQ:           dlq,
			Resolver:      entry.config.Resolver,
			Processors:    entry.config.Processors,
			Bindings:      entry.config.Bindings,
			InstanceID:    rt.instanceID,
			Metrics:       m,
			Tracer:        rt.tracer,
			Logger:        rt.logger,
			GlobalSem:     rt.globalSem,
			DepthCacheTTL: entry.config.Policy.DepthCacheTTL,
		})

		if entry.session != nil && entry.sessCfg != nil {
			sid := entry.sessCfg.SessionID
			if _, exists := rt.sessionMgrs[sid]; !exists {
				mgr := newSessionManagerWithMetrics(*entry.sessCfg, entry.session, rt.leaseStore, rt.instanceID, rt.logger, m)
				mgr.SetAudit(rt.audit)
				mgr.endpoints = rt.clusterEndpoints
				rt.sessionMgrs[sid] = mgr
			}

			if entry.sessCfg.Exclusive && rt.locator != nil {
				rt.locator.RegisterRoute(entry.config.ID, sid)
			}

			if entry.config.Policy.DeliveryMode == domain.DeliverySharedOutbox && rt.outboxStore != nil && !drainerSessions[sid] {
				drainerSessions[sid] = true
				mgr := rt.sessionMgrs[sid]
				sess := entry.session
				drainer := newOutboxDrainer(OutboxDrainerConfig{
					OutboxStore:    rt.outboxStore,
					LeaseStore:     rt.leaseStore,
					Sender:         entry.sender,
					DLQ:            dlq,
					RouteID:        entry.config.ID,
					PartitionKey:   domain.OutboxPartitionKey(sid, ""),
					LeaseID:        sid,
					OwnerID:        rt.instanceID,
					Policy:         entry.config.Policy.WithDefaults(),
					Strategy:            entry.sessCfg.DrainStrategy,
					DrainBatchSize:      entry.sessCfg.DrainBatchSize,
					DrainMaxBatchSize:   entry.sessCfg.DrainMaxBatchSize,
					DrainMaxConcurrency: entry.sessCfg.DrainMaxConcurrency,
					Metrics:             m,
					Logger:         rt.logger,
					TokenFn:        mgr.Token,
					ReadyFn: func() bool {
						return sess.Health(context.Background()).Connected
					},
				})
				rt.drainers = append(rt.drainers, drainer)
			}
		}

		// For SharedOutbox routes, create drainers for every target
		// session referenced by bindings that was not already covered
		// by the route's primary session.
		if entry.config.Policy.DeliveryMode == domain.DeliverySharedOutbox && rt.outboxStore != nil {
			for _, binding := range entry.config.Bindings {
				sid := binding.SessionID
				if sid == "" || drainerSessions[sid] {
					continue
				}

				sse, ok := rt.sessionSenders[sid]
				if !ok {
					continue
				}

				if _, exists := rt.sessionMgrs[sid]; !exists {
					mgr := newSessionManagerWithMetrics(sse.config, sse.session, rt.leaseStore, rt.instanceID, rt.logger, m)
					mgr.SetAudit(rt.audit)
					mgr.endpoints = rt.clusterEndpoints
					rt.sessionMgrs[sid] = mgr
				}

				drainerSessions[sid] = true
				mgr := rt.sessionMgrs[sid]
				fanSess := sse.session
				drainer := newOutboxDrainer(OutboxDrainerConfig{
					OutboxStore:    rt.outboxStore,
					LeaseStore:     rt.leaseStore,
					Sender:         sse.sender,
					DLQ:            dlq,
					RouteID:        entry.config.ID,
					PartitionKey:   domain.OutboxPartitionKey(sid, ""),
					LeaseID:        sid,
					OwnerID:        rt.instanceID,
					Policy:         entry.config.Policy.WithDefaults(),
					Strategy:            sse.config.DrainStrategy,
					DrainBatchSize:      sse.config.DrainBatchSize,
					DrainMaxBatchSize:   sse.config.DrainMaxBatchSize,
					DrainMaxConcurrency: sse.config.DrainMaxConcurrency,
					Metrics:             m,
					Logger:         rt.logger,
					TokenFn:        mgr.Token,
					ReadyFn: func() bool {
						return fanSess.Health(context.Background()).Connected
					},
				})
				rt.drainers = append(rt.drainers, drainer)
			}
		}
	}

	for sid, mgr := range rt.sessionMgrs {
		rt.startBackground(ctx, "session:"+sid, mgr.Run)
	}

	for i, drainer := range rt.drainers {
		d := drainer
		name := "drainer:" + d.partitionKey
		if name == "drainer:" {
			name = "drainer:" + d.routeID + ":" + strconv.Itoa(i)
		}
		rt.startBackground(ctx, name, d.Run)
	}

	for _, entry := range rt.entries {
		e := entry
		rt.startBackground(ctx, "route:"+e.config.ID, e.runner.Run)
	}

	return nil
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

	logging.Debug(rt.logger, "runtime stopping",
		"instance_id", rt.instanceID,
	)

	if cancel != nil {
		cancel()
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

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer flushCancel()
		if err := metrics.Flush(flushCtx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// RouteInfo describes a registered route for introspection.
type RouteInfo struct {
	ID           string
	DeliveryMode domain.DeliveryMode
	DispatchMode domain.DispatchMode
	Policy       domain.RoutePolicy
}

// InstanceID returns the bridge instance identifier.
func (rt *Runtime) InstanceID() string {
	return rt.instanceID
}

// RouteLocator returns the cluster-aware route locator.
// Returns nil if no lease store is configured (standalone mode).
func (rt *Runtime) RouteLocator() ports.RouteLocator {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.locator == nil {
		return nil
	}
	return rt.locator
}

// IsRunning reports whether the runtime is currently running and healthy.
// Returns false if any critical background component has failed.
func (rt *Runtime) IsRunning() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.running && rt.healthy
}

// Healthy reports whether all background components are healthy.
func (rt *Runtime) Healthy() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.healthy
}

// ComponentErrors returns a copy of the failed component error map.
func (rt *Runtime) ComponentErrors() map[string]error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cp := make(map[string]error, len(rt.componentErrors))
	for k, v := range rt.componentErrors {
		cp[k] = v
	}
	return cp
}

// Routes returns information about all registered routes.
func (rt *Runtime) Routes() []RouteInfo {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	infos := make([]RouteInfo, len(rt.entries))
	for i, e := range rt.entries {
		infos[i] = RouteInfo{
			ID:           e.config.ID,
			DeliveryMode: e.config.Policy.DeliveryMode,
			DispatchMode: e.config.Policy.DispatchMode,
			Policy:       e.config.Policy,
		}
	}
	return infos
}

// DeepHealth returns a comprehensive health snapshot including session
// subscription readiness and lease status. Use ReadyForTraffic to
// determine if all sessions are connected and subscribed before
// sending messages through the bridge.
func (rt *Runtime) DeepHealth(ctx context.Context) DeepHealth {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	dh := DeepHealth{
		Running:    rt.running,
		Healthy:    rt.healthy,
		InstanceID: rt.instanceID,
		Role:       rt.roleUnlocked(),
	}

	allReady := rt.running && rt.healthy
	aggSL := ports.ServiceLevelFull

	// Collect session health from route entries.
	seen := make(map[string]bool)
	for _, e := range rt.entries {
		if e.session == nil || e.sessCfg == nil {
			continue
		}
		sid := e.sessCfg.SessionID
		if seen[sid] {
			continue
		}
		seen[sid] = true
		sh := e.session.Health(ctx)

		detail := SessionHealthDetail{
			SessionID:           sid,
			Connected:           sh.Connected,
			SubscriptionsWanted: sh.SubscriptionsWanted,
			SubscriptionsActive: sh.SubscriptionsActive,
			Ready:               sh.Ready,
			ServiceLevel:        sh.ServiceLevel,
		}
		if mgr, ok := rt.sessionMgrs[sid]; ok {
			_, detail.HasLease = mgr.Token()
		}
		dh.Sessions = append(dh.Sessions, detail)
		if !sh.Ready {
			allReady = false
		}
		aggSL = minServiceLevel(aggSL, sh.ServiceLevel)
	}

	// Also include sessionSenders (fan-out targets).
	for sid, sse := range rt.sessionSenders {
		if seen[sid] {
			continue
		}
		seen[sid] = true
		sh := sse.session.Health(ctx)
		detail := SessionHealthDetail{
			SessionID:           sid,
			Connected:           sh.Connected,
			SubscriptionsWanted: sh.SubscriptionsWanted,
			SubscriptionsActive: sh.SubscriptionsActive,
			Ready:               sh.Ready,
			ServiceLevel:        sh.ServiceLevel,
		}
		if mgr, ok := rt.sessionMgrs[sid]; ok {
			_, detail.HasLease = mgr.Token()
		}
		dh.Sessions = append(dh.Sessions, detail)
		if !sh.Ready {
			allReady = false
		}
		aggSL = minServiceLevel(aggSL, sh.ServiceLevel)
	}

	// Routes.
	for _, e := range rt.entries {
		dh.Routes = append(dh.Routes, RouteHealth{
			ID:           e.config.ID,
			DeliveryMode: string(e.config.Policy.DeliveryMode),
		})
	}

	dh.ReadyForTraffic = allReady
	dh.ServiceLevel = aggSL
	return dh
}

// minServiceLevel returns the lower of two service levels.
// Order: None < Degraded < Full. Empty string is treated as None.
func minServiceLevel(a, b ports.ServiceLevel) ports.ServiceLevel {
	order := map[ports.ServiceLevel]int{
		ports.ServiceLevelNone:     0,
		"":                         0,
		ports.ServiceLevelDegraded: 1,
		ports.ServiceLevelFull:     2,
	}
	if order[a] < order[b] {
		return a
	}
	return b
}

// DeepHealth is a comprehensive health snapshot of the runtime.
type DeepHealth struct {
	Running         bool
	Healthy         bool
	InstanceID      string
	Role            string
	Routes          []RouteHealth
	Sessions        []SessionHealthDetail
	ReadyForTraffic bool               // All sessions connected + runtime healthy
	ServiceLevel    ports.ServiceLevel // Minimum service level across all sessions
}

// SessionHealthDetail describes one session's health including
// subscription readiness and lease ownership.
type SessionHealthDetail struct {
	SessionID           string
	Connected           bool
	HasLease            bool
	SubscriptionsWanted int
	SubscriptionsActive int
	Ready               bool
	ServiceLevel        ports.ServiceLevel
}

// RouteHealth describes one route's health.
type RouteHealth struct {
	ID           string
	DeliveryMode string
}

// roleUnlocked returns the role without acquiring the mutex (caller must hold it).
func (rt *Runtime) roleUnlocked() string {
	if len(rt.sessionMgrs) == 0 {
		return "standalone"
	}
	for _, mgr := range rt.sessionMgrs {
		if _, held := mgr.Token(); held {
			return "active"
		}
	}
	return "standby"
}

// LeaseStatus returns the lease ownership status for each exclusive
// session. The map keys are session IDs; values are true when the
// session holds the lease (active) and false otherwise (standby).
// Non-exclusive sessions are not included.
func (rt *Runtime) LeaseStatus() map[string]bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	status := make(map[string]bool)
	for id, mgr := range rt.sessionMgrs {
		_, held := mgr.Token()
		status[id] = held
	}
	return status
}

// Role returns the operational role of this instance based on lease
// ownership: "active" if at least one exclusive session holds a lease,
// "standby" if exclusive sessions exist but none hold a lease,
// "standalone" if no exclusive sessions are configured.
// The result is a point-in-time snapshot computed under a single lock.
func (rt *Runtime) Role() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.roleUnlocked()
}

// DLQStore returns the DLQ store if configured, or nil.
func (rt *Runtime) DLQStore() ports.DLQStore {
	return rt.dlqStore
}

// Inject sends a synthetic message through the named route's delivery
// pipeline (processors, destination resolution, send/outbox). The
// envelope is cloned to prevent caller mutation. An ID is assigned if
// the envelope's ID field is empty.
func (rt *Runtime) Inject(ctx context.Context, routeID string, env *domain.Envelope) error {
	rt.mu.Lock()
	if !rt.running {
		rt.mu.Unlock()
		return fmt.Errorf("runtime is not running")
	}
	var entry *routeEntry
	for _, e := range rt.entries {
		if e.config.ID == routeID {
			entry = e
			break
		}
	}
	rt.mu.Unlock()

	if entry == nil {
		return domain.ErrNotFound
	}

	env = env.Clone()
	if env.ID == "" {
		env.ID = generateID()
	}

	return entry.runner.handleDelivery(ctx, &syntheticDelivery{env: env})
}

// syntheticDelivery implements ports.Delivery for programmatically
// injected messages that have no underlying transport.
type syntheticDelivery struct {
	env *domain.Envelope
}

func (d *syntheticDelivery) Envelope() *domain.Envelope  { return d.env }
func (d *syntheticDelivery) Ack(_ context.Context) error { return nil }
func (d *syntheticDelivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	return domain.ErrNotSupported
}
func (d *syntheticDelivery) Extend(_ context.Context, _ time.Time) error { return nil }

func (rt *Runtime) startBackground(ctx context.Context, name string, fn func(context.Context) error) {
	logging.Trace(rt.logger, "background started", "name", name)
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
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
	}()
}

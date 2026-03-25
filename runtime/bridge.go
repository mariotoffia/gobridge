package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Runtime is the top-level coordinator for the GoBridge message routing
// engine. It manages routes, sessions, and outbox drainers.
// Start returns immediately after spawning background goroutines.
// Stop cancels the context, waits for all goroutines to finish, then
// closes sessions.
type Runtime struct {
	instanceID  string
	leaseStore  ports.LeaseStore
	outboxStore ports.OutboxStore
	dlqStore    ports.DLQStore
	logger      *slog.Logger

	mu              sync.Mutex
	entries         []*routeEntry
	sessionSenders  map[string]*sessionSenderEntry
	sessionMgrs     map[string]*SessionManager
	drainers        []*OutboxDrainer
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

// WithLogger sets the structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(rt *Runtime) { rt.logger = logger }
}

// New creates a new Runtime with the given options.
func New(opts ...Option) *Runtime {
	rt := &Runtime{
		instanceID:     generateID(),
		sessionSenders: make(map[string]*sessionSenderEntry),
		sessionMgrs:    make(map[string]*SessionManager),
		healthy:        true,
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
	if rt.running {
		rt.mu.Unlock()
		return errors.New("runtime already running")
	}
	rt.running = true
	rt.healthy = true
	rt.componentErrors = make(map[string]error)

	if err := validateRoutes(rt.entries, rt.outboxStore != nil, rt.leaseStore != nil); err != nil {
		rt.running = false
		rt.mu.Unlock()
		return err
	}

	ctx, rt.cancel = context.WithCancel(ctx)

	dlq := NewDLQRouter(rt.dlqStore)

	drainerSessions := make(map[string]bool)

	for _, entry := range rt.entries {
		entry.runner = newRouteRunner(RouteRunnerConfig{
			RouteID:     entry.config.ID,
			Policy:      entry.config.Policy,
			Receiver:    entry.receiver,
			Sender:      entry.sender,
			OutboxStore: rt.outboxStore,
			DLQ:         dlq,
			Resolver:    entry.config.Resolver,
			Processors:  entry.config.Processors,
			Bindings:    entry.config.Bindings,
			InstanceID:  rt.instanceID,
			Logger:      rt.logger,
		})

		if entry.session != nil && entry.sessCfg != nil {
			sid := entry.sessCfg.SessionID
			if _, exists := rt.sessionMgrs[sid]; !exists {
				mgr := newSessionManager(*entry.sessCfg, entry.session, rt.leaseStore, rt.instanceID, rt.logger)
				rt.sessionMgrs[sid] = mgr
			}

			if entry.config.Policy.DeliveryMode == domain.DeliverySharedOutbox && rt.outboxStore != nil && !drainerSessions[sid] {
				drainerSessions[sid] = true
				mgr := rt.sessionMgrs[sid]
				drainer := newOutboxDrainer(OutboxDrainerConfig{
					OutboxStore:   rt.outboxStore,
					LeaseStore:    rt.leaseStore,
					Sender:        entry.sender,
					DLQ:           dlq,
					RouteID:       entry.config.ID,
					PartitionKey:  domain.OutboxPartitionKey(sid, ""),
					LeaseID:       sid,
					OwnerID:       rt.instanceID,
					Policy:        entry.config.Policy.WithDefaults(),
					DrainInterval: entry.sessCfg.DrainInterval,
					BatchSize:     entry.sessCfg.DrainBatchSize,
					Logger:        rt.logger,
					TokenFn:       mgr.Token,
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
					mgr := newSessionManager(sse.config, sse.session, rt.leaseStore, rt.instanceID, rt.logger)
					rt.sessionMgrs[sid] = mgr
				}

				drainerSessions[sid] = true
				mgr := rt.sessionMgrs[sid]
				drainer := newOutboxDrainer(OutboxDrainerConfig{
					OutboxStore:   rt.outboxStore,
					LeaseStore:    rt.leaseStore,
					Sender:        sse.sender,
					DLQ:           dlq,
					RouteID:       entry.config.ID,
					PartitionKey:  domain.OutboxPartitionKey(sid, ""),
					LeaseID:       sid,
					OwnerID:       rt.instanceID,
					Policy:        entry.config.Policy.WithDefaults(),
					DrainInterval: sse.config.DrainInterval,
					BatchSize:     sse.config.DrainBatchSize,
					Logger:        rt.logger,
					TokenFn:       mgr.Token,
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
			name = "drainer:" + d.routeID + ":" + string(rune('0'+i))
		}
		rt.startBackground(ctx, name, d.Run)
	}

	for _, entry := range rt.entries {
		e := entry
		rt.startBackground(ctx, "route:"+e.config.ID, e.runner.Run)
	}

	rt.mu.Unlock()
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

	rt.mu.Lock()
	for _, mgr := range rt.sessionMgrs {
		if err := mgr.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	rt.mu.Unlock()

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

// DLQStore returns the DLQ store if configured, or nil.
func (rt *Runtime) DLQStore() ports.DLQStore {
	return rt.dlqStore
}

func (rt *Runtime) startBackground(ctx context.Context, name string, fn func(context.Context) error) {
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if rt.logger != nil {
				rt.logger.Error("background component failed", "component", name, "error", err)
			}
			rt.mu.Lock()
			rt.healthy = false
			rt.componentErrors[name] = err
			cancel := rt.cancel
			rt.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	}()
}

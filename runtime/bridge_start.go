package runtime

import (
	"context"
	"errors"
	"strconv"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

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

	if logging.DebugEnabled(rt.logger) {
		rt.logger.Log(ctx, logging.LevelDebug, "runtime starting",
			"instance_id", rt.instanceID,
			"route_count", len(rt.entries),
			"session_count", len(rt.sessionMgrs)+len(rt.sessionSenders),
		)
	}

	ctx, rt.cancel = context.WithCancel(ctx)

	dlq := NewDLQRouterFromConfig(DLQRouterConfig{Store: rt.dlqStore, Clock: rt.clk})
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
		rt.locator = newRouteLocator(rt.instanceID, rt.leaseStore, DefaultRouteLocatorConfig(), rt.clk)
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
			Hook:          rt.hook,
			Logger:        rt.logger,
			GlobalSem:     rt.globalSem,
			DepthCacheTTL: entry.config.Policy.DepthCacheTTL,
			Clock:         rt.clk,
		})

		if rt.locator != nil {
			if setter, ok := entry.receiver.(interface{ SetRouteID(string) }); ok {
				setter.SetRouteID(entry.config.ID)
			}
			if setter, ok := entry.sender.(interface{ SetRouteID(string) }); ok {
				setter.SetRouteID(entry.config.ID)
			}
		}

		if entry.session != nil && entry.sessCfg != nil {
			sid := entry.sessCfg.SessionID
			if _, exists := rt.sessionMgrs[sid]; !exists {
				mgr := newSessionManagerWithMetrics(*entry.sessCfg, entry.session, rt.leaseStore, rt.instanceID, rt.logger, m, rt.clk)
				mgr.SetAudit(rt.audit)
				mgr.endpoints = rt.clusterEndpoints
				rt.sessionMgrs[sid] = mgr
			}

			if entry.sessCfg.Exclusive && rt.locator != nil {
				rt.locator.RegisterRoute(entry.config.ID, sid)
			}

			if entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox && rt.outboxStore != nil && !drainerSessions[sid] {
				drainerSessions[sid] = true
				mgr := rt.sessionMgrs[sid]
				sess := entry.session
				drainer := newOutboxDrainer(OutboxDrainerConfig{
					OutboxStore:           rt.outboxStore,
					LeaseStore:            rt.leaseStore,
					Sender:                entry.sender,
					DLQ:                   dlq,
					RouteID:               entry.config.ID,
					PartitionKey:          persistence.OutboxPartitionKey(sid, ""),
					LeaseID:               sid,
					OwnerID:               rt.instanceID,
					Policy:                entry.config.Policy.WithDefaults(),
					Strategy:              entry.sessCfg.DrainStrategy,
					DrainBatchSize:        entry.sessCfg.DrainBatchSize,
					DrainMaxBatchSize:     entry.sessCfg.DrainMaxBatchSize,
					DrainMaxConcurrency:   entry.sessCfg.DrainMaxConcurrency,
					DrainTimeout:          entry.sessCfg.DrainTimeout,
					PerRecordDrainTimeout: entry.sessCfg.PerRecordDrainTimeout,
					MaxDrainTimeout:       entry.sessCfg.MaxDrainTimeout,
					Metrics:               m,
					Hook:                  rt.hook,
					Logger:                rt.logger,
					TokenFn:               mgr.Token,
					Clock:                 rt.clk,
					ReadyFn: func(ctx context.Context) bool {
						return sess.Health(ctx).Connected
					},
				})
				rt.drainers = append(rt.drainers, drainer)
			}
		}

		// For SharedOutbox routes, create drainers for every target
		// session referenced by bindings that was not already covered
		// by the route's primary session.
		if entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox && rt.outboxStore != nil {
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
					mgr := newSessionManagerWithMetrics(sse.config, sse.session, rt.leaseStore, rt.instanceID, rt.logger, m, rt.clk)
					mgr.SetAudit(rt.audit)
					mgr.endpoints = rt.clusterEndpoints
					rt.sessionMgrs[sid] = mgr
				}

				drainerSessions[sid] = true
				mgr := rt.sessionMgrs[sid]
				fanSess := sse.session
				drainer := newOutboxDrainer(OutboxDrainerConfig{
					OutboxStore:           rt.outboxStore,
					LeaseStore:            rt.leaseStore,
					Sender:                sse.sender,
					DLQ:                   dlq,
					RouteID:               entry.config.ID,
					PartitionKey:          persistence.OutboxPartitionKey(sid, ""),
					LeaseID:               sid,
					OwnerID:               rt.instanceID,
					Policy:                entry.config.Policy.WithDefaults(),
					Strategy:              sse.config.DrainStrategy,
					DrainBatchSize:        sse.config.DrainBatchSize,
					DrainMaxBatchSize:     sse.config.DrainMaxBatchSize,
					DrainMaxConcurrency:   sse.config.DrainMaxConcurrency,
					DrainTimeout:          sse.config.DrainTimeout,
					PerRecordDrainTimeout: sse.config.PerRecordDrainTimeout,
					MaxDrainTimeout:       sse.config.MaxDrainTimeout,
					Metrics:               m,
					Hook:                  rt.hook,
					Logger:                rt.logger,
					TokenFn:               mgr.Token,
					Clock:                 rt.clk,
					ReadyFn: func(ctx context.Context) bool {
						return fanSess.Health(ctx).Connected
					},
				})
				rt.drainers = append(rt.drainers, drainer)
			}
		}
	}

	if len(rt.sessionMgrs) > 0 {
		mgrs := rt.sessionMgrs
		dlq.SetTokenFn(func() (persistence.LeaseToken, bool) {
			for _, mgr := range mgrs {
				if tok, held := mgr.Token(); held {
					return tok, true
				}
			}
			return persistence.LeaseToken{}, false
		})
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

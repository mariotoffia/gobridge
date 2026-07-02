package runtime

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/cluster"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/runtime/session"
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
	rt.terminal = false
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

	// The DLQ write is on the settle-critical path: it runs inside the route
	// runner's per-delivery goroutine, which holds the global in-flight slot
	// until it returns. Keep the budget small and well under typical source
	// visibility so a degraded DLQ store cannot pin global slots for the full
	// 30s×3 default and starve healthy routes. Worst case here is ~10.5s.
	//
	// With the default transports this budget does not affect duplicates: SQS
	// AutoExtend and ASB lock renewal keep the source message invisible for the
	// whole blocking write, so a hung store yields a clean NACK-and-retry, not a
	// duplicate DLQ entry. Duplicate amplification only arises if a source runs
	// with visibility extension OFF and a visibility window shorter than the
	// send+DLQ budget — a deployment that must size visibility accordingly.
	//
	// ponytail: fixed 2×5s budget; make it per-route visibility-aware only if a
	// route both disables visibility extension and sets a visibility window below
	// the send+DLQ budget.
	dlqRouter := dlq.NewFromConfig(dlq.Config{
		Store:            rt.dlqStore,
		Clock:            rt.clk,
		WriteTimeout:     5 * time.Second,
		WriteMaxAttempts: 2,
	})

	m := rt.metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}

	if rt.globalMaxInFlight > 0 {
		rt.globalSem = make(chan struct{}, rt.globalMaxInFlight)
	}

	if rt.leaseStore != nil {
		rt.locator = cluster.NewLocator(rt.instanceID, rt.leaseStore, cluster.DefaultLocatorConfig(), rt.clk)
	}

	drainerSessions := make(map[string]bool)

	for _, entry := range rt.entries {
		// A1: For shared_outbox routes with a primary session, bindings that
		// omit their own SessionID inherit the route session. This keeps each
		// outbox record's partition (SESSION#<routeSession>) aligned with the
		// drainer that polls it; without this, records persist under
		// BINDING#<id> while the only drainer polls SESSION#<routeSession>, so
		// they never drain even though the source was ACKed after persist.
		if entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox && entry.sessCfg != nil {
			for i := range entry.config.Bindings {
				if entry.config.Bindings[i].SessionID == "" {
					entry.config.Bindings[i].SessionID = entry.sessCfg.SessionID
				}
			}
		}

		entry.runner = route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
			RouteID:           entry.config.ID,
			Policy:            entry.config.Policy,
			Receiver:          entry.receiver,
			Sender:            entry.sender,
			Senders:           entry.config.Senders,
			AddressValidators: entry.config.AddressValidators,
			OutboxStore:       rt.outboxStore,
			DLQ:               dlqRouter,
			Resolver:          entry.config.Resolver,
			Processors:        entry.config.Processors,
			Bindings:          entry.config.Bindings,
			InstanceID:        rt.instanceID,
			Metrics:           m,
			Tracer:            rt.tracer,
			Hook:              rt.hook,
			Logger:            rt.logger,
			GlobalSem:         rt.globalSem,
			DepthCacheTTL:     entry.config.Policy.DepthCacheTTL,
			Clock:             rt.clk,
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
				mgr := session.NewWithMetrics(*entry.sessCfg, entry.session, rt.leaseStore, rt.instanceID, rt.logger, m, rt.clk)
				mgr.SetAudit(rt.audit)
				mgr.SetEndpoints(rt.clusterEndpoints)
				rt.sessionMgrs[sid] = mgr
			}

			if entry.sessCfg.Exclusive && rt.locator != nil {
				rt.locator.RegisterRoute(entry.config.ID, sid)
			}

			if entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox && rt.outboxStore != nil && !drainerSessions[sid] {
				drainerSessions[sid] = true
				mgr := rt.sessionMgrs[sid]
				sess := entry.session
				drainer := outbox.New(outbox.Config{
					OutboxStore:           rt.outboxStore,
					LeaseStore:            rt.leaseStore,
					Sender:                entry.sender,
					DLQ:                   dlqRouter,
					RouteID:               entry.config.ID,
					PartitionKey:          persistence.OutboxPartitionKey(sid, ""),
					LeaseID:               sid,
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
					mgr := session.NewWithMetrics(sse.config, sse.session, rt.leaseStore, rt.instanceID, rt.logger, m, rt.clk)
					mgr.SetAudit(rt.audit)
					mgr.SetEndpoints(rt.clusterEndpoints)
					rt.sessionMgrs[sid] = mgr
				}

				drainerSessions[sid] = true
				mgr := rt.sessionMgrs[sid]
				fanSess := sse.session
				drainer := outbox.New(outbox.Config{
					OutboxStore:           rt.outboxStore,
					LeaseStore:            rt.leaseStore,
					Sender:                sse.sender,
					DLQ:                   dlqRouter,
					RouteID:               entry.config.ID,
					PartitionKey:          persistence.OutboxPartitionKey(sid, ""),
					LeaseID:               sid,
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
		dlqRouter.SetTokenFn(func() (persistence.LeaseToken, bool) {
			for _, mgr := range mgrs {
				if tok, held := mgr.Token(); held {
					return tok, true
				}
			}
			return persistence.LeaseToken{}, false
		})
	}

	for sid, mgr := range rt.sessionMgrs {
		rt.startBackground(ctx, "session:"+sid, rt.superviseSession(sid, mgr.Run))
	}

	for i, drainer := range rt.drainers {
		name := "drainer:" + drainer.PartitionKey()
		if name == "drainer:" {
			name = "drainer:" + drainer.RouteID() + ":" + strconv.Itoa(i)
		}
		rt.startBackground(ctx, name, drainer.Run)
	}

	for _, entry := range rt.entries {
		rt.startBackground(ctx, "route:"+entry.config.ID, entry.runner.Run)
	}

	return nil
}

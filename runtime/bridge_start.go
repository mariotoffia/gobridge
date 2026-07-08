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

// ValidateRoutes runs pre-start route validation and returns a
// [ValidationError] describing every problem found, or nil when all routes are
// valid. It is idempotent and side-effect-free (it never mutates route entries
// or runtime state), so it is safe to call repeatedly and BEFORE Start — the
// builder's complete() calls it at the end of construction (C2). Start invokes
// the same validation internally, so a runtime that fails ValidateRoutes also
// fails Start.
func (rt *Runtime) ValidateRoutes() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return validateRoutes(rt.entries, rt.outboxStore != nil, rt.leaseStore != nil, rt.dlqStore != nil)
}

// Start wires up all registered routes, session managers, and outbox
// drainers, then spawns background goroutines. It returns immediately;
// use Stop to shut down gracefully.
func (rt *Runtime) Start(ctx context.Context) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.terminal || rt.stopped {
		// Finding 3: Stop closes the outbox/DLQ/lease stores and cancels every
		// drainer/manager, but the drainers/managers/entries are never rebuilt.
		// A restart would append fresh drainers over CLOSED stores and duplicate
		// work. The runtime is single-use — for BOTH a clean deliberate Stop
		// (rt.stopped) and an unrecoverable component failure (rt.terminal):
		// reject a restart with a clear error. "Resume" after a deliberate stop
		// means the supervisor builds a NEW runtime (CRITICAL 1).
		return errors.New("runtime: cannot start a stopped runtime (single-use lifecycle); build a new runtime")
	}

	if rt.running {
		return errors.New("runtime already running")
	}
	rt.running = true
	rt.healthy = true
	rt.terminal = false
	rt.componentErrors = make(map[string]error)
	rt.routeFlaps = make(map[string]int)
	rt.routeRunStart = make(map[string]time.Time)

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

	// Watch the caller-supplied Start context. If it is cancelled WITHOUT a Stop
	// (the caller cancels the ctx it passed to Start, rather than calling Stop),
	// every background goroutine exits on the derived ctx — but running/healthy
	// stay advertised, leaving a dead runtime that still reports healthy on /live
	// and ready on /ready (finding L9). Drive Stop so resources are released and
	// health flips. A Stop that itself cancelled the ctx already set terminal, so
	// this observes terminal and does nothing (no double teardown). The watcher
	// is deliberately NOT in rt.wg (Stop waits on rt.wg, so enrolling it would
	// deadlock); it always terminates because ctx is cancelled by either Stop or
	// the caller.
	watchCtx := ctx
	go func() {
		<-watchCtx.Done()
		rt.mu.Lock()
		stopping := rt.terminal || !rt.running
		rt.mu.Unlock()
		if stopping {
			return
		}
		stopBudget := rt.shutdownTimeout
		if stopBudget <= 0 {
			stopBudget = 5 * time.Second
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), stopBudget)
		defer cancel()
		_ = rt.Stop(stopCtx)
	}()

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
	m := rt.metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}

	dlqRouter := dlq.NewFromConfig(dlq.Config{
		Store:            rt.dlqStore,
		Clock:            rt.clk,
		WriteTimeout:     5 * time.Second,
		WriteMaxAttempts: 2,
		// H2: wire Metrics/Logger so MetricDLQWriteFailures reaches the real
		// exporter (not a NoopExporter) and router write errors are logged.
		// Without these the production DLQ router was blind.
		Metrics: m,
		Logger:  rt.logger,
	})

	if rt.globalMaxInFlight > 0 {
		rt.globalSem = make(chan struct{}, rt.globalMaxInFlight)
	}

	if rt.leaseStore != nil {
		rt.locator = cluster.NewLocator(rt.leaseOwnerID, rt.leaseStore, cluster.DefaultLocatorConfig(), rt.clk)
	}

	// drainerOwner maps a session ID to the route whose configuration
	// (policy, sender, RouteID) its shared-outbox drainer was built from.
	// Exactly one drainer exists per session partition, so when SEVERAL
	// shared_outbox routes reference the same session, every route's records
	// drain under the FIRST route's configuration — a silent config bleed
	// (send timeouts, replay budget, drain strategy, metrics route tag).
	// warnDrainerConfigBleed surfaces it.
	drainerOwner := make(map[string]string)
	warnDrainerConfigBleed := func(sid, owner, routeID string) {
		if owner == routeID || rt.logger == nil {
			return
		}
		rt.logger.Warn("shared outbox drainer config bleed: session drainer was built from another route's policy/sender; this route's records drain under that configuration",
			"session_id", sid,
			"drainer_route_id", owner,
			"route_id", routeID,
		)
	}

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
			SourceTransport:   entry.config.SourceTransport,
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
				mgr := session.NewWithMetrics(*entry.sessCfg, entry.session, rt.leaseStore, rt.leaseOwnerID, rt.logger, m, rt.clk)
				mgr.SetAudit(rt.audit)
				mgr.SetEndpoints(rt.clusterEndpoints)
				rt.sessionMgrs[sid] = mgr
			}

			if entry.sessCfg.Exclusive && rt.locator != nil {
				rt.locator.RegisterRoute(entry.config.ID, sid)
			}

			if entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox && rt.outboxStore != nil {
				if owner, exists := drainerOwner[sid]; exists {
					warnDrainerConfigBleed(sid, owner, entry.config.ID)
				} else {
					drainerOwner[sid] = entry.config.ID
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
						PoisonMinAge:          rt.outboxPoisonMinAge,
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
					// F9: let step-down early-complete its grace when this
					// session's outbox has no in-flight records to settle.
					mgr.SetDrainIdleCheck(func() bool { _, idle := drainer.IdleSince(); return idle })
				}
			}
		}

		// For SharedOutbox routes, create drainers for every target
		// session referenced by bindings that was not already covered
		// by the route's primary session.
		if entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox && rt.outboxStore != nil {
			for _, binding := range entry.config.Bindings {
				sid := binding.SessionID
				if sid == "" {
					continue
				}
				if owner, exists := drainerOwner[sid]; exists {
					warnDrainerConfigBleed(sid, owner, entry.config.ID)
					continue
				}

				sse, ok := rt.sessionSenders[sid]
				if !ok {
					continue
				}

				if _, exists := rt.sessionMgrs[sid]; !exists {
					mgr := session.NewWithMetrics(sse.config, sse.session, rt.leaseStore, rt.leaseOwnerID, rt.logger, m, rt.clk)
					mgr.SetAudit(rt.audit)
					mgr.SetEndpoints(rt.clusterEndpoints)
					rt.sessionMgrs[sid] = mgr
				}

				drainerOwner[sid] = entry.config.ID
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
					PoisonMinAge:          rt.outboxPoisonMinAge,
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
				// F9: let step-down early-complete its grace when this session's
				// outbox has no in-flight records to settle.
				mgr.SetDrainIdleCheck(func() bool { _, idle := drainer.IdleSince(); return idle })
			}
		}
	}

	// Finding 12: DLQ writes are fenced PER OWNING SESSION, not by an
	// instance-global "any lease held" gate. Build the set of exclusive
	// sessions (only they carry a lease) so the router can decide per DLQ entry:
	//   - empty sessionID (ingress failure with no owning session): allow — no
	//     lease governs it.
	//   - non-exclusive session (not in the set): allow — there is no lease to
	//     fence on, so a standby may DLQ-write its own ingress failures.
	//   - exclusive session managed here: gate on THAT session's live lease, so a
	//     standby that does not own the lease cannot DLQ (and an unrelated lease
	//     cannot authorize a write for a route it does not own).
	//   - exclusive session NOT managed here: refuse — the owning instance writes
	//     the entry, avoiding a cross-instance duplicate.
	exclusiveSessions := make(map[string]bool)
	for _, entry := range rt.entries {
		if entry.sessCfg != nil && entry.sessCfg.Exclusive {
			exclusiveSessions[entry.sessCfg.SessionID] = true
		}
	}
	for sid, sse := range rt.sessionSenders {
		if sse.config.Exclusive {
			exclusiveSessions[sid] = true
		}
	}
	if len(rt.sessionMgrs) > 0 {
		mgrs := rt.sessionMgrs
		dlqRouter.SetTokenFn(func(sessionID string) (persistence.LeaseToken, bool) {
			if sessionID == "" {
				return persistence.LeaseToken{}, true
			}
			if !exclusiveSessions[sessionID] {
				return persistence.LeaseToken{}, true
			}
			if mgr, ok := mgrs[sessionID]; ok {
				return mgr.Token()
			}
			return persistence.LeaseToken{}, false
		})
	}

	// Session managers run under superviseSession (NOT bare startBackground): a
	// transient session fault — including a reconcile-on-reconnect blip
	// (session/manager.go handleSessionEvent, session/manager_lease.go
	// afterRenewLoopExit) — restarts JUST this session with capped backoff
	// instead of tearing down the whole runtime. A PERMANENT reconcile failure
	// (e.g. an ACL that keeps rejecting SUBSCRIBE) is likewise not escalated to
	// a pod restart; it stays observable via MetricReconcileFailures +
	// MetricSessionRestarts + per-session readiness. This supersedes the old
	// "reconcile blip terminates the whole bridge" behaviour, so no extra
	// in-manager reconcile retry is added: it would duplicate this isolation and
	// risk masking a permanent failure behind another retry layer (C7-N1).
	for sid, mgr := range rt.sessionMgrs {
		rt.startBackground(ctx, "session:"+sid, rt.superviseSession(sid, mgr.Run))
	}

	// Drainers run under startBackground (terminal-on-error) — correct AS-IS:
	// Drainer.Run only ever returns ctx.Err() (every drain fault — stale token,
	// transient egress, claim failure — is absorbed and retried inside its own
	// poll loop, see runtime/outbox/loop.go). A non-ctx return is therefore
	// impossible, so a transient drain fault can never trip terminal, and the
	// ctx.Err() shutdown return is filtered by startBackground's ctx.Err()==nil
	// guard. No supervisor wrapper is warranted (REV-3-routeiso).
	for i, drainer := range rt.drainers {
		name := "drainer:" + drainer.PartitionKey()
		if name == "drainer:" {
			name = "drainer:" + drainer.RouteID() + ":" + strconv.Itoa(i)
		}
		rt.startBackground(ctx, name, drainer.Run)
	}

	// Route runners run under superviseRoute (per-route isolation) — NOT the
	// terminal-on-error startBackground the sessions' pre-fix path used. A
	// runtime hosts MANY routes, and a fault that is permanent for ONE route
	// (its source queue deleted, its credential revoked, a protocol mismatch on
	// that link) is NOT a global fault: crashing the whole pod would punish every
	// healthy co-tenant route and, since the fault is permanent, just
	// CrashLoopBackOff without fixing anything (findings C1-MED, C2-HIGH). This
	// supersedes the former REV-3-routeiso fail-fast rationale (which assumed
	// every receiver error is global-and-unrecoverable); superviseRoute isolates
	// the failing route with jittered capped backoff, keeps global healthy/
	// terminal untouched, and keeps the fault observable via MetricRouteRestarts
	// + failed_components + per-route readiness. See superviseRoute for the full
	// weighing of the replaced argument and its honest tradeoff (single-use
	// receivers settle at the backoff cap rather than reconnect).
	for _, entry := range rt.entries {
		rt.startBackground(ctx, "route:"+entry.config.ID, rt.superviseRoute(entry.config.ID, entry.runner.Run))
	}

	return nil
}

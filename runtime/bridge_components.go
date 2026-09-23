package runtime

import (
	"context"
	"strconv"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// componentRun is one background component started under its own child of the
// runtime's work context, so it can be stopped alone.
type componentRun struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// startComponent runs fn under a child of ctx via startBackground (so rt.wg
// and the terminal-on-error contract are unchanged) and returns its handle.
// Cancelling the handle ends fn under a cancelled context, which
// startBackground reads as a clean stop, never as a component failure.
func (rt *Runtime) startComponent(ctx context.Context, name string, fn func(context.Context) error) componentRun {
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.startBackground(cctx, name, func(c context.Context) error {
		defer close(done)
		return fn(c)
	})
	return componentRun{cancel: cancel, done: done}
}

// drainerRun pairs a drainer with the session partition it drains and its run.
type drainerRun struct {
	drainer   *outbox.Drainer
	sessionID string
	run       componentRun
}

// componentSet is what one wiring pass starts: Start passes rt's own
// collections, Graft passes the part's.
type componentSet struct {
	entries         []*routeEntry
	sessionSenders  map[string]*sessionSenderEntry
	ingressSessions map[string]*ingressSessionEntry
}

// startComponentsLocked wires and starts set: route runners, session managers
// (only for session ids set introduces), drainers, settlement barriers, the
// removed-subscription dead-letter path, locator registration and the
// exclusive-session set. Caller holds rt.mu and rt.workCtx is set.
//
// The collections of set must already be registered in rt: the dead-letter
// path resolves a session and its source route through rt's collections.
func (rt *Runtime) startComponentsLocked(set componentSet) {
	ctx := rt.workCtx
	m := rt.metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	firstDrainer := len(rt.drainers)

	// Source route settlement barriers are installed on sessions after every
	// RouteRunner exists and before any background goroutine starts.
	settlementSessions := make(map[string]ports.Session)
	settlementRoutes := make(map[string][]*routeEntry)

	// created is every session id whose manager this pass builds. Only those
	// managers are started and given the dead-letter path: a manager that
	// already exists belongs to whatever built it, which owns its run.
	created := rt.wireRouteEntriesLocked(m, set, settlementSessions, settlementRoutes)

	// Every registered session sender gets a manager, whatever delivery mode
	// the routes that reach it use. The shared-outbox wiring above creates one
	// because it needs a drainer; a direct_hold route that names the session on
	// a binding needs one just as much — a plan-driven session (MQTT, AMQP
	// 0-9-1) connects and subscribes only when a manager reconciles its plan,
	// and the builder admits that binding precisely as the way to get one. A
	// session sender left without a manager is a session that never connects,
	// a receiver that never subscribes, and a bridge that reports ready while
	// transporting nothing. Such a session is also the INGRESS of every route
	// whose receiver rides on it, so it joins settlementSessions below and
	// gets the same settlement barrier a route-primary session gets before it
	// recycles a broker connection.
	for sid, sse := range set.sessionSenders {
		if rt.ensureSessionManagerLocked(m, sid, sse.config, sse.session) {
			created = append(created, sid)
		}
		for _, entry := range set.entries {
			ridesOn := entry.config.SourceSessionID == sid ||
				(entry.sessCfg == nil && entry.session == sse.session)
			if !ridesOn {
				continue
			}
			settlementSessions[sid] = sse.session
			settlementRoutes[sid] = append(settlementRoutes[sid], entry)
		}
	}

	// An ingress session carries only its receivers' subscriptions: it gets a
	// plain manager (no lease, no drainer) and the settlement barrier for the
	// routes riding on it.
	created = append(created, rt.attachIngressSessions(m, set.ingressSessions, set.entries, settlementSessions, settlementRoutes)...)

	for sid, sess := range settlementSessions {
		configurer, ok := sess.(ports.IngressQuiescenceConfigurer)
		if !ok {
			continue
		}
		// The barrier waits on the entries captured here, not on a lookup by
		// route id: a reload may retire these routes and start successors under
		// the same ids, and a retired session must never wait on, or be released
		// by, its successor's runners.
		entries := settlementRoutes[sid]
		configurer.SetIngressQuiescenceWaiter(func(waitCtx context.Context) error {
			return rt.waitEntriesQuiescent(waitCtx, func() []*routeEntry { return entries }, QuiescenceOptions{
				// Router ingress is already closed, so no additional quiet window
				// is required; only accepted RouteRunner settlements matter.
				MinQuiet: -1,
			})
		})
	}

	rt.installRemovedSubscriptionDeadLetter(rt.dlqRouter, created)

	// Only an exclusive session carries a lease, so only its DLQ writes are
	// fenced (see dlqToken).
	for _, entry := range set.entries {
		if entry.sessCfg != nil && entry.sessCfg.Exclusive {
			rt.exclusiveSessions[entry.sessCfg.SessionID] = true
		}
	}
	for sid, sse := range set.sessionSenders {
		if sse.config.Exclusive {
			rt.exclusiveSessions[sid] = true
		}
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
	// risk masking a permanent failure behind another retry layer.
	for _, sid := range created {
		rt.sessionRuns[sid] = rt.startComponent(ctx, "session:"+sid, rt.superviseSession(sid, rt.sessionMgrs[sid].Run))
	}

	// Drainers run under startBackground (terminal-on-error). Every RECOVERABLE
	// drain fault — stale token, transient egress, claim failure — is absorbed and
	// retried inside the poll loop (runtime/outbox/loop.go), so the normal return
	// is ctx.Err() (filtered by startBackground's ctx.Err()==nil guard). The one
	// deliberate non-ctx return is outbox.ErrDrainStalled: a Sender that ignores
	// context cancellation leaks a goroutine the batch watchdog can only abandon,
	// so Run stops draining and returns terminal to trigger a restart that
	// reclaims it — the escalation the terminal-on-error path exists for. No
	// per-drainer supervisor wrapper is warranted.
	for i := firstDrainer; i < len(rt.drainers); i++ {
		d := rt.drainers[i]
		name := "drainer:" + d.drainer.PartitionKey()
		if name == "drainer:" {
			name = "drainer:" + d.drainer.RouteID() + ":" + strconv.Itoa(i)
		}
		d.run = rt.startComponent(ctx, name, d.drainer.Run)
	}

	// Route runners run under superviseRoute (per-route isolation) — NOT the
	// terminal-on-error startBackground. A runtime hosts MANY routes, and a
	// fault that is permanent for ONE route (its source queue deleted, its
	// credential revoked, a protocol mismatch on that link) is NOT a global
	// fault: crashing the whole pod would punish every healthy co-tenant route
	// and, since the fault is permanent, just CrashLoopBackOff without fixing
	// anything. superviseRoute isolates the failing route with jittered capped
	// backoff, keeps global healthy/terminal untouched, and keeps the fault
	// observable via MetricRouteRestarts + failed_components + per-route
	// readiness. See superviseRoute for the full weighing of the replaced
	// argument and its honest tradeoff (single-use receivers settle at the
	// backoff cap rather than reconnect).
	for _, entry := range set.entries {
		entry.run = rt.startComponent(ctx, "route:"+entry.config.ID, rt.superviseRoute(entry.config.ID, entry.runner.Run))
	}

	rt.logBestEffortSubscriptions(set.entries)
}

// wireRouteEntriesLocked builds the route runner of every entry in set, gives
// each route-primary session its manager and locator registration, builds the
// shared-outbox drainers, and records which sessions each route's receiver
// rides on for the settlement barriers. It returns the ids of the session
// managers it built. Caller holds rt.mu.
func (rt *Runtime) wireRouteEntriesLocked(
	m ports.MetricsExporter,
	set componentSet,
	settlementSessions map[string]ports.Session,
	settlementRoutes map[string][]*routeEntry,
) (created []string) {
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

	for _, entry := range set.entries {
		sharedOutbox := entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox
		// For shared_outbox routes with a primary session, bindings that
		// omit their own SessionID inherit the route session. This keeps each
		// outbox record's partition (SESSION#<routeSession>) aligned with the
		// drainer that polls it; without this, records persist under
		// BINDING#<id> while the only drainer polls SESSION#<routeSession>, so
		// they never drain even though the source was ACKed after persist.
		if sharedOutbox && entry.sessCfg != nil {
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
			DLQ:               rt.dlqRouter,
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
			settlementSessions[sid] = entry.session
			settlementRoutes[sid] = append(settlementRoutes[sid], entry)
			if rt.ensureSessionManagerLocked(m, sid, *entry.sessCfg, entry.session) {
				created = append(created, sid)
			}

			if entry.sessCfg.Exclusive && rt.locator != nil {
				rt.locator.RegisterRoute(entry.config.ID, sid)
			}

			if sharedOutbox && rt.outboxStore != nil {
				if owner, exists := drainerOwner[sid]; exists {
					warnDrainerConfigBleed(sid, owner, entry.config.ID)
				} else {
					drainerOwner[sid] = entry.config.ID
					rt.addDrainerLocked(m, entry, sid, entry.sessCfg, entry.session, entry.sender)
				}
			}
		}

		// For SharedOutbox routes, create drainers for every target
		// session referenced by bindings that was not already covered
		// by the route's primary session.
		if !sharedOutbox || rt.outboxStore == nil {
			continue
		}
		for _, binding := range entry.config.Bindings {
			sid := binding.SessionID
			if sid == "" {
				continue
			}
			if owner, exists := drainerOwner[sid]; exists {
				warnDrainerConfigBleed(sid, owner, entry.config.ID)
				continue
			}
			sse, ok := set.sessionSenders[sid]
			if !ok {
				continue
			}
			if rt.ensureSessionManagerLocked(m, sid, sse.config, sse.session) {
				created = append(created, sid)
			}
			drainerOwner[sid] = entry.config.ID
			rt.addDrainerLocked(m, entry, sid, &sse.config, sse.session, sse.sender)
		}
	}
	return created
}

// ensureSessionManagerLocked gives session sid a manager unless it already has
// one, and reports whether it built one: a session has exactly one manager,
// built from whichever of its registrations is wired first. Caller holds rt.mu.
func (rt *Runtime) ensureSessionManagerLocked(m ports.MetricsExporter, sid string, cfg session.Config, sess ports.Session) bool {
	if _, exists := rt.sessionMgrs[sid]; exists {
		return false
	}
	mgr := session.NewWithMetrics(cfg, sess, rt.leaseStore, rt.leaseOwnerID, rt.logger, m, rt.clk)
	mgr.SetAudit(rt.audit)
	mgr.SetEndpoints(rt.clusterEndpoints)
	rt.sessionMgrs[sid] = mgr
	return true
}

// addDrainerLocked builds the shared-outbox drainer of session partition sid —
// under entry's route id and policy, sending through sender, tuned by cfg — and
// appends it to rt.drainers unstarted. The session's manager must exist. Caller
// holds rt.mu.
func (rt *Runtime) addDrainerLocked(
	m ports.MetricsExporter,
	entry *routeEntry,
	sid string,
	cfg *session.Config,
	sess ports.Session,
	sender ports.Sender,
) {
	mgr := rt.sessionMgrs[sid]
	drainer := outbox.New(outbox.Config{
		OutboxStore:           rt.outboxStore,
		LeaseStore:            rt.leaseStore,
		Sender:                sender,
		DLQ:                   rt.dlqRouter,
		RouteID:               entry.config.ID,
		PartitionKey:          persistence.OutboxPartitionKey(sid, ""),
		LeaseID:               sid,
		Policy:                entry.config.Policy.WithDefaults(),
		Strategy:              cfg.DrainStrategy,
		DrainBatchSize:        cfg.DrainBatchSize,
		DrainMaxBatchSize:     cfg.DrainMaxBatchSize,
		DrainMaxConcurrency:   cfg.DrainMaxConcurrency,
		PerRecordDrainTimeout: cfg.PerRecordDrainTimeout,
		MaxDrainTimeout:       cfg.MaxDrainTimeout,
		Metrics:               m,
		Hook:                  rt.hook,
		Logger:                rt.logger,
		TokenFn:               mgr.Token,
		Clock:                 rt.clk,
		ReadyFn: func(ctx context.Context) bool {
			return sess.Health(ctx).Connected
		},
	})
	rt.drainers = append(rt.drainers, &drainerRun{drainer: drainer, sessionID: sid})
	// let step-down early-complete its grace when this session's outbox has no
	// in-flight records to settle.
	mgr.SetDrainIdleCheck(func() bool { _, idle := drainer.IdleSince(); return idle })
}

// dlqToken is the DLQ router's token function. DLQ writes are fenced PER
// OWNING SESSION, not by an instance-global "any lease held" gate:
//   - empty sessionID (ingress failure with no owning session): allow — no
//     lease governs it.
//   - non-exclusive session (not in rt.exclusiveSessions): allow — there is no
//     lease to fence on, so a standby may DLQ-write its own ingress failures.
//   - exclusive session managed here: gate on THAT session's live lease, so a
//     standby that does not own the lease cannot DLQ (and an unrelated lease
//     cannot authorize a write for a route it does not own). A manager a Retire
//     is still winding down counts: its lease is held until it closes.
//   - exclusive session NOT managed here: refuse — the owning instance writes
//     the entry, avoiding a cross-instance duplicate.
//
// It reads the managers and the exclusive set under rt.mu, because routes and
// sessions may be added to or removed from a running runtime, but asks the
// manager for its token only after releasing the lock.
func (rt *Runtime) dlqToken(sessionID string) (persistence.LeaseToken, bool) {
	if sessionID == "" {
		return persistence.LeaseToken{}, true
	}
	rt.mu.Lock()
	exclusive := rt.exclusiveSessions[sessionID]
	mgr, managed := rt.sessionMgrs[sessionID]
	if !managed {
		mgr, managed = rt.retiringManagerLocked(sessionID)
	}
	rt.mu.Unlock()
	if !exclusive {
		return persistence.LeaseToken{}, true
	}
	if managed {
		return mgr.Token()
	}
	return persistence.LeaseToken{}, false
}

func (rt *Runtime) logBestEffortSubscriptions(entries []*routeEntry) {
	if rt.logger == nil {
		return
	}
	for _, entry := range entries {
		if entry.config.Policy.WithDefaults().DeliveryMode != routing.DeliveryDirectHold {
			continue
		}
		for _, topic := range entry.config.SourceBestEffortTopics {
			rt.logger.Info("direct_hold subscription is best-effort; QoS 0 messages may be lost if the process stops",
				"route_id", entry.config.ID,
				"receiver_id", entry.config.SourceReceiverID,
				"topic", topic,
			)
		}
	}
}

package bridge

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// complete creates sessions, receivers, senders, wires routes, and
// returns a ready-to-start Runtime. Callers must ensure the old
// runtime has released exclusive resources (e.g. MQTT client-ids)
// before invoking this phase.
//
// complete is unexported; external callers reach it through
// Builder.Build (single-shot) or BuildPlan.Commit (explicit
// two-phase). See M-3 / W-7.
func (b *Builder) complete(ctx context.Context, prep *preparedBuild) (_ *runtime.Runtime, retErr error) {
	if prep == nil {
		return nil, fmt.Errorf("bridge: complete called with nil preparedBuild")
	}

	sessions, sessionURIs, err := b.buildSessionsWithURIs(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			for id, s := range sessions {
				if closeErr := s.Close(ctx); closeErr != nil && b.logger != nil {
					b.logger.Warn("closing session after build failure", "session", id, "error", closeErr)
				}
			}
		}
	}()

	// If the build fails after the runtime takes ownership of the prep-opened
	// stores, the discarded runtime is never Started and therefore never
	// Stopped, so its lease/outbox/DLQ handles (e.g. SQLite files) would leak on
	// every failed swap (Finding 2). Release them here, mirroring runtime.Stop's
	// io.Closer teardown. Sessions are handled by the defer above; the durable
	// store handles are the piece an abandoned, never-started runtime would
	// otherwise never close. This is independent of the supervisor calling
	// newRt.Stop() on a runtime it received (C1): complete() returns nil on
	// failure, so the supervisor never sees this runtime to stop it.
	defer func() {
		if retErr != nil {
			b.closeStoreHandles(prep.stores)
		}
	}()

	receivers, receiverURIs, err := b.buildReceiversWithURIs(ctx, sessions)
	if err != nil {
		return nil, err
	}

	senders, senderURIs, err := b.buildSendersWithURIs(ctx, sessions)
	if err != nil {
		return nil, err
	}

	rt := runtime.New(prep.rtOpts...)

	if err := b.wireRoutes(rt, sessions, receivers, senders); err != nil {
		return nil, err
	}

	// Start credential refresh watchers for any session, receiver, or
	// sender that carries a credentials_uri AND whose target implements
	// CredentialAware. Gated on the effective push store so builds without
	// one skip this entirely, preserving legacy behavior. effectivePushStore
	// resolves an explicitly-registered push store, or lazily wraps a polled
	// pull store with the fully-resolved logger (Finding 13).
	pushStore := b.effectivePushStore()
	if pushStore != nil && (len(sessionURIs)+len(receiverURIs)+len(senderURIs)) > 0 {
		var refresherOpts []RefresherOption
		// Wire push rotations to the pull-cache invalidator so a rotation
		// observed by the refresher drops the stale cached entry and the next
		// synchronous resolve fetches fresh material (contract C4). The pull
		// store is the CredentialResolver when the composition root wires one;
		// detect the capability structurally to avoid importing runtime here.
		if inv, ok := b.credStore.(interface{ InvalidateCache(uri string) }); ok {
			refresherOpts = append(refresherOpts, WithRotationCallback(inv.InvalidateCache))
		}
		refresher := NewCredentialRefresher(pushStore, b.logger, refresherOpts...)
		for sid, uri := range sessionURIs {
			if sess, ok := sessions[sid]; ok {
				refresher.Watch(ctx, uri, sess)
			}
		}
		for rid, uri := range receiverURIs {
			if recv, ok := receivers[rid]; ok {
				refresher.WatchReceiver(ctx, uri, recv)
			}
		}
		for sid, uri := range senderURIs {
			if snd, ok := senders[sid]; ok {
				refresher.WatchSender(ctx, uri, snd)
			}
		}
		rt.AttachCredentialCloser(func(_ context.Context) { refresher.Close() })
	}

	// Contract C2: run the runtime's pre-start route validation now, while the
	// OLD runtime (if any) is still serving, so a statically-rejectable config
	// fails HERE — during construction, before the supervisor stops the old
	// runtime — instead of inside Start, after the old runtime has already been
	// torn down and cannot resume. ValidateRoutes is idempotent and
	// side-effect-free; Start runs the same checks internally as a backstop. On
	// failure retErr is set, so the defers above release the sessions and store
	// handles this half-built runtime opened (Finding 2).
	if err := rt.ValidateRoutes(); err != nil {
		return nil, fmt.Errorf("bridge: route validation: %w", err)
	}

	return rt, nil
}

// closeStoreHandles releases any store handles that hold OS resources (SQLite
// file handles, network connections) when a build is abandoned before the
// runtime that would own them is Started. In-memory stores do not implement
// io.Closer and are skipped, mirroring runtime.Stop's teardown. The order
// (outbox, DLQ, lease) matches runtime.Stop for consistency; each Close is
// best-effort and a failure is logged rather than propagated because the build
// has already failed and every handle must still be attempted.
func (b *Builder) closeStoreHandles(stores *storeResult) {
	if stores == nil {
		return
	}
	for _, s := range []any{stores.outbox, stores.dlq, stores.lease} {
		c, ok := s.(io.Closer)
		if !ok {
			continue
		}
		if err := c.Close(); err != nil && b.logger != nil {
			b.logger.Warn("closing store after build failure", "error", err)
		}
	}
}

// (contract C5). Instead of a hard-coded session.DefaultConfig — which pinned a
// ~6-minute failover regardless of a tuned cluster — it inherits the timings
// from the route's own session block when present, the same source the route's
// primary session uses, so a binding-only exclusive sender is tuned like the
// rest of the deployment. When the route has no session block it falls back to
// defaults but leaves RenewInterval unset so the session manager derives it
// from LeaseTTL (contract C3), and applies bridge-level drain defaults.
func (b *Builder) bindingSessionConfig(routeDef ports.RouteDef, sessionID string) (session.Config, error) {
	if routeDef.Session != nil {
		derived, err := toSessionConfigE(routeDef.Session)
		if err != nil {
			return session.Config{}, err
		}
		sc := *derived
		sc.SessionID = sessionID
		applyBridgeDrainDefaults(&sc, b.cfg.Bridge)
		return sc, nil
	}
	sc := session.DefaultConfig(sessionID, true)
	sc.RenewInterval = 0
	applyBridgeDrainDefaults(&sc, b.cfg.Bridge)
	return sc, nil
}

func (b *Builder) wireRoutes(
	rt *runtime.Runtime,
	sessions map[string]ports.Session,
	receivers map[string]ports.Receiver,
	senders map[string]ports.Sender,
) error {
	if err := validateSharedOutboxBindingSessions(b.cfg); err != nil {
		return err
	}

	registeredSessions := make(map[string]bool)

	for _, routeDef := range b.cfg.Routes {
		recv, ok := receivers[routeDef.ReceiverID]
		if !ok {
			return fmt.Errorf("bridge: route %q: receiver %q not created", routeDef.ID, routeDef.ReceiverID)
		}

		bindings := toBindings(b.cfg, routeDef.Bindings)
		policy, policyErr := toRoutePolicyE(routeDef)
		if policyErr != nil {
			return fmt.Errorf("bridge: route %q: %w", routeDef.ID, policyErr)
		}
		sessCfg, sessCfgErr := toSessionConfigE(routeDef.Session)
		if sessCfgErr != nil {
			return fmt.Errorf("bridge: route %q: %w", routeDef.ID, sessCfgErr)
		}
		applyBridgeDrainDefaults(sessCfg, b.cfg.Bridge)

		// Assemble the session's desired topology from the blueprint so the
		// session manager reconciles a non-empty plan. sessionPlanFor is the
		// per-session union of every receiver bound to the session, so the
		// plan is identical for all routes sharing it — safe under the
		// first-wins session-manager dedup in runtime/bridge_start.go.
		// Without this the broker session declares no topology and
		// subscribes to nothing (F1). sessCfg is nil only when the route has
		// no session, in which case there is nothing to reconcile.
		if sessCfg != nil && routeDef.Session != nil {
			sessCfg.Plan = sessionPlanFor(b.cfg, routeDef.Session.SessionID, b.logger)
		}

		var routeSession ports.Session
		var routeSender ports.Sender
		var caps []ports.Capability
		var sourceVisTimeout time.Duration
		var sourceAutoExtend bool

		recvDef := findReceiver(b.cfg, routeDef.ReceiverID)
		if recvDef != nil {
			transport := recvDef.Transport
			if transport == "" {
				if sd := findSession(b.cfg, recvDef.SessionID); sd != nil {
					transport = sd.Transport
				}
			}
			if tf, ok := b.transports[transport]; ok {
				caps = tf.Capabilities()
				if vtp, ok := tf.(ports.VisibilityTimeoutProvider); ok {
					sourceVisTimeout = vtp.VisibilityTimeout()
				}
				// A per-route receiver config (SQS visibility_timeout, ASB
				// lock_duration) overrides the transport-wide Factory
				// constant, so the validator checks SendTimeout against the
				// window the route actually runs with (Finding 2 / D2). Its
				// auto-extend flag lets the validator skip that check when
				// the window is renewed in the background.
				if vc, ok := recvDef.Config.(ports.VisibilityTimeoutConfig); ok {
					sourceVisTimeout = vc.EffectiveVisibilityTimeout()
					sourceAutoExtend = vc.AutoExtendEnabled()
				}
			}
		}

		if routeDef.Session != nil {
			sid := routeDef.Session.SessionID
			if s, ok := sessions[sid]; ok {
				routeSession = s
			}
			if snd, ok := senders[routeDef.Session.SenderID]; ok {
				routeSender = snd
			} else {
				return fmt.Errorf("bridge: route %q: session sender %q not created", routeDef.ID, routeDef.Session.SenderID)
			}
		} else if len(bindings) > 0 {
			firstBind := bindings[0]
			if snd, ok := senders[firstBind.SenderID]; ok {
				routeSender = snd
			}
			if firstBind.SessionID != "" {
				if s, ok := sessions[firstBind.SessionID]; ok {
					routeSession = s
				}
			}
		}

		if routeSender == nil {
			return fmt.Errorf("bridge: route %q: no sender resolved", routeDef.ID)
		}

		// A shared_outbox route drains its outbox through the drainer wired to
		// its primary session. If that session resolves to nil because it is
		// declared on a stateless transport, the runtime creates no drainer for
		// the partition: the source is ACKed after the outbox persist but the
		// records are never drained — silent message loss (Finding 4). Reject
		// it at build time.
		if routeDef.Session != nil && routeSession == nil &&
			routing.DeliveryMode(routeDef.DeliveryMode) == routing.DeliverySharedOutbox {
			transport := ""
			if sd := findSession(b.cfg, routeDef.Session.SessionID); sd != nil {
				transport = sd.Transport
			}
			return fmt.Errorf("bridge: route %q: shared_outbox route declares primary session %q but "+
				"it resolves to a nil session (transport %q is stateless); a shared_outbox route needs a "+
				"stateful session to drain its outbox, otherwise records are persisted and never drained "+
				"(silent message loss) — use a stateful transport for the session or a direct delivery mode",
				routeDef.ID, routeDef.Session.SessionID, transport)
		}

		procs, procErr := b.resolveProcessors(routeDef.Processors)
		if procErr != nil {
			return fmt.Errorf("bridge: route %q: %w", routeDef.ID, procErr)
		}

		rcfg := runtime.RouteConfig{
			ID:                      routeDef.ID,
			Policy:                  policy,
			Bindings:                bindings,
			Processors:              procs,
			SourceCapabilities:      caps,
			SourceVisibilityTimeout: sourceVisTimeout,
			SourceAutoExtend:        sourceAutoExtend,
		}

		// Build content-based resolver from config if present.
		if routeDef.Resolver != nil {
			resolver, resolverErr := buildResolver(routeDef.Resolver, bindings)
			if resolverErr != nil {
				return fmt.Errorf("bridge: route %q: resolver: %w", routeDef.ID, resolverErr)
			}
			rcfg.Resolver = resolver
		}

		// Build per-binding sender registry for DirectHold multi-sender dispatch.
		if len(bindings) > 1 {
			senderReg := make(map[string]ports.Sender, len(bindings))
			for _, bd := range bindings {
				snd, ok := senders[bd.SenderID]
				if !ok {
					return fmt.Errorf("bridge: route %q: binding %q references unknown sender %q",
						routeDef.ID, bd.ID, bd.SenderID)
				}
				senderReg[bd.ID] = snd
			}
			rcfg.Senders = senderReg
		}

		// Build per-binding AddressValidator registry. The validator is
		// supplied by the binding's transport via TransportFactory's
		// AddressValidator capability (AP-005). Bindings whose transport
		// returns a nil validator are simply omitted; the runtime then
		// skips validation for those bindings.
		if vmap := buildAddressValidators(b.transports, bindings); len(vmap) > 0 {
			rcfg.AddressValidators = vmap
		}

		// Build-time address validation (D1): fail fast when a binding's static
		// address does not match its sender's bound destination, instead of
		// surfacing the error only at first send. Only literal addresses are
		// checked here — an address containing a "{key}" placeholder is rendered
		// per message (runtime/route.RenderAddress) and cannot be validated
		// statically. Config-built resolvers select among these same bindings,
		// so a literal address is a valid check with or without a resolver; this
		// also covers bindings only reachable through a resolver, each validated
		// against its own sender. Senders that do not implement
		// ports.AddressValidatingSender (or cannot decide statically) are skipped
		// and rely on send-time validation.
		for _, bd := range bindings {
			if bd.Address == "" || strings.Contains(bd.Address, "{") {
				continue
			}
			av, ok := senders[bd.SenderID].(ports.AddressValidatingSender)
			if !ok {
				continue
			}
			if err := av.ValidateAddress(bd.Address); err != nil {
				return fmt.Errorf("bridge: route %q: binding %q: %w", routeDef.ID, bd.ID, err)
			}
		}

		if err := rt.AddRoute(rcfg, recv, routeSender, routeSession, sessCfg); err != nil {
			return fmt.Errorf("bridge: add route %q: %w", routeDef.ID, err)
		}

		if routeDef.Session != nil {
			registeredSessions[routeDef.Session.SessionID] = true
		}

		for _, bd := range bindings {
			if bd.SessionID == "" || registeredSessions[bd.SessionID] {
				continue
			}
			sess, sessOk := sessions[bd.SessionID]
			if !sessOk {
				if decl := findSession(b.cfg, bd.SessionID); decl != nil {
					return fmt.Errorf("bridge: route %q: binding %q references session %q whose "+
						"transport %q is stateless (it creates no session object); a binding drainer "+
						"needs a stateful session — records would be persisted and never drained "+
						"(silent message loss)", routeDef.ID, bd.ID, bd.SessionID, decl.Transport)
				}
				return fmt.Errorf("bridge: route %q: binding %q references unknown session %q", routeDef.ID, bd.ID, bd.SessionID)
			}
			snd, sndOk := senders[bd.SenderID]
			if !sndOk {
				return fmt.Errorf("bridge: route %q: binding %q references unknown sender %q", routeDef.ID, bd.ID, bd.SenderID)
			}
			// Derive the binding session's lease timings the same way the
			// route's primary session gets them, instead of a hard-coded
			// DefaultConfig that pinned a ~6-minute failover on an otherwise
			// tuned cluster (contract C5).
			sc, scErr := b.bindingSessionConfig(routeDef, bd.SessionID)
			if scErr != nil {
				return fmt.Errorf("bridge: route %q: binding %q session config: %w", routeDef.ID, bd.ID, scErr)
			}
			sc.ConnectAfterLease = true
			// Thread the session's desired topology so a session registered only
			// via a binding (Path-2) still reconciles its receivers' subscriptions
			// and sender exchanges instead of an empty plan (F1-P4). Mirrors the
			// Path-1 assignment above. bd.SessionID is non-empty (guarded above).
			sc.Plan = sessionPlanFor(b.cfg, bd.SessionID, b.logger)
			if err := rt.RegisterSessionSender(sc, sess, snd); err != nil {
				return fmt.Errorf("bridge: register session sender %q: %w", bd.SessionID, err)
			}
			registeredSessions[bd.SessionID] = true
		}
	}

	// registeredSessions now holds every session the builder wired with a
	// manager (route-primary Path-1 + session-sender Path-2). A plan-driven
	// receiver bound to a session absent from this set would never reconcile and
	// be silently inert — fail the build instead (ADV-P4-FU1).
	if err := b.validatePlanDrivenReceiverSessions(registeredSessions); err != nil {
		return err
	}

	return nil
}

// validatePlanDrivenReceiverSessions fails the build when a receiver on a
// PLAN-DRIVEN transport (one advertising ports.CapPlanDrivenSubscriptions —
// mqtt, amqp091) is bound to a session that gets NO session manager. Such a
// session is never reconciled, so the receiver's subscriptions are never
// established and it is silently inert (ADV-P4-FU1, the missing-manager sibling
// of ADV-F1-P4).
//
// managed is the set of sessions wired with a manager during wireRoutes — every
// route-primary session (Path-1) and every session-sender session (Path-2). A
// session manager is created ONLY from one of those two wirings (see
// runtime/bridge_start.go), so a plan-driven receiver whose session is absent
// from this set can never have its plan reconciled.
//
// Self-establishing (amqp10, whose receivers attach links on start independent
// of the plan) and address-direct (SQS/Service Bus/HTTP) transports do NOT
// advertise the capability and are skipped: for them a missing manager is not
// the same silent-loss defect. This is exactly why ADV-F1-P4 could not be fixed
// with a blanket validate-layer guard — it false-positived on amqp10, which the
// config layer cannot distinguish without adapter knowledge arch-lint forbids.
func (b *Builder) validatePlanDrivenReceiverSessions(managed map[string]bool) error {
	// Only a receiver actually referenced by a route is wired and can subscribe;
	// an unreferenced receiver is inert regardless of its session, so it is not
	// this defect and is not flagged here.
	routed := make(map[string]bool, len(b.cfg.Routes))
	for i := range b.cfg.Routes {
		routed[b.cfg.Routes[i].ReceiverID] = true
	}
	for i := range b.cfg.Receivers {
		rd := &b.cfg.Receivers[i]
		if rd.SessionID == "" || managed[rd.SessionID] || !routed[rd.ID] {
			continue
		}
		transport := rd.Transport
		if transport == "" {
			if sd := findSession(b.cfg, rd.SessionID); sd != nil {
				transport = sd.Transport
			}
		}
		tf, ok := b.transports[transport]
		if !ok || !slices.Contains(tf.Capabilities(), ports.CapPlanDrivenSubscriptions) {
			continue
		}
		return fmt.Errorf("bridge: receiver %q (transport %q) is bound to session %q, which no route "+
			"manages, so its plan-driven subscriptions would never reconcile and it would be silently "+
			"inert; give a route a session block naming %q (or a binding targeting it) so the session is "+
			"managed and its subscriptions are established",
			rd.ID, transport, rd.SessionID, rd.SessionID)
	}
	return nil
}

// validateSharedOutboxBindingSessions rejects a shared_outbox binding whose
// session is the PRIMARY session of a DIFFERENT route. Within a single
// blueprint every binding session must be drained locally: there is no
// "another instance drains it" pattern in one blueprint (cross-instance handoff
// uses distinct per-instance configs built through the low-level API, not one
// shared blueprint). A binding session is correctly drained only when it is
// either (i) the route's own primary session — drained by that route's Path-1
// outbox drainer — or (ii) a dedicated session owned by no route, registered as
// a session-sender (Path-2). A session that is some OTHER route's primary is a
// topology error:
//
//   - other route is direct_hold: that session has NO outbox drainer at all, so
//     the binding's records persist under SESSION#<sid> and are never drained —
//     the source is ACKed after persist and the record is silently lost.
//   - other route is shared_outbox: that session is drained by the OTHER route's
//     sender, so the binding's records are delivered to the wrong destination.
//
// A dedicated session is also a topology error when two bindings drain it with
// DIFFERENT senders: a session has exactly one Path-2 drainer, wired with the
// FIRST registered binding's sender, so the later binding's records drain via
// the wrong sender and are delivered to the wrong destination. (Two bindings
// sharing a session with the SAME sender is the legitimate fan-out case and is
// allowed.)
//
// Symmetrically, a binding whose EFFECTIVE session is the route's OWN primary
// (named explicitly, or inherited via the empty-SessionID rule) is drained by
// the route's single Path-1 drainer, which is wired with the route primary's
// sender. A binding that names a DIFFERENT sender there has that sender silently
// ignored — the records ship through the route sender to the wrong destination.
// Reject it so the footgun is explicit rather than silent. (An empty binding
// SenderID is the normal case: it simply uses the route sender.)
//
// wireRoutes resolves the primary-collision case via a builder-wide
// registeredSessions map whose outcome is declaration-order dependent (a
// direct_hold primary declared first suppresses the session-sender registration
// a later shared_outbox binding needs; reverse the order and the binding instead
// contends for the exclusive lease) and silently drops the second sender in the
// sender-conflict case. Validate it statically up front, where the whole topology
// is known and order does not matter.
func validateSharedOutboxBindingSessions(cfg *ports.BridgeConfig) error {
	type primary struct {
		routeID      string
		sharedOutbox bool
	}
	primaryOf := make(map[string]primary, len(cfg.Routes))
	for i := range cfg.Routes {
		rd := &cfg.Routes[i]
		if rd.Session == nil || rd.Session.SessionID == "" {
			continue
		}
		primaryOf[rd.Session.SessionID] = primary{
			routeID:      rd.ID,
			sharedOutbox: routing.DeliveryMode(rd.DeliveryMode) == routing.DeliverySharedOutbox,
		}
	}

	dedicatedSender := make(map[string]string, len(cfg.Routes))
	for i := range cfg.Routes {
		rd := &cfg.Routes[i]
		if routing.DeliveryMode(rd.DeliveryMode) != routing.DeliverySharedOutbox {
			continue
		}
		ownPrimary := ""
		if rd.Session != nil {
			ownPrimary = rd.Session.SessionID
		}
		for _, bd := range toBindings(cfg, rd.Bindings) {
			// Own-primary conflicting sender: when a binding's effective
			// session is the route's own primary (named explicitly, or
			// inherited via the empty-SessionID rule), its records drain
			// through the route's single Path-1 drainer, wired with the route
			// primary's sender. A different non-empty binding SenderID is
			// silently ignored, so the records ship through the wrong sender.
			// rd.Session.SenderID=="" is left to wireRoutes' "session sender
			// not created" error; an empty binding SenderID is the normal
			// "use the route sender" case.
			if ownPrimary != "" && rd.Session.SenderID != "" &&
				(bd.SessionID == ownPrimary || bd.SessionID == "") &&
				bd.SenderID != "" && bd.SenderID != rd.Session.SenderID {
				return fmt.Errorf("bridge: route %q: binding %q resolves to the route's primary "+
					"session %q but names sender %q instead of the session sender %q; the primary "+
					"session has a single outbox drainer wired with the route sender, so the binding "+
					"sender is silently ignored and its records are delivered through the wrong sender "+
					"— omit sender_id to use the route sender, or give this binding a dedicated session_id",
					rd.ID, bd.ID, ownPrimary, bd.SenderID, rd.Session.SenderID)
			}
			if bd.SessionID == "" || bd.SessionID == ownPrimary {
				continue
			}
			owner, ok := primaryOf[bd.SessionID]
			if ok && owner.routeID != rd.ID {
				if owner.sharedOutbox {
					return fmt.Errorf("bridge: route %q: binding %q targets session %q, which is the "+
						"primary session of shared_outbox route %q; its outbox records would be drained by "+
						"route %q's sender and delivered to the wrong destination — give this binding a "+
						"dedicated session_id",
						rd.ID, bd.ID, bd.SessionID, owner.routeID, owner.routeID)
				}
				return fmt.Errorf("bridge: route %q: binding %q targets session %q, which is the primary "+
					"session of direct_hold route %q; a direct_hold session has no outbox drainer, so the "+
					"binding's records would persist under SESSION#%s and never drain (silent message loss) "+
					"— give this binding a dedicated session_id",
					rd.ID, bd.ID, bd.SessionID, owner.routeID, bd.SessionID)
			}
			if ok {
				continue // self-reference; already covered by the ownPrimary skip
			}
			// Dedicated fan-out session (no route's primary). It gets a single
			// Path-2 drainer wired with the FIRST binding's sender; a later binding
			// draining the same session with a different sender is silently dropped
			// by the builder's registeredSessions dedup and mis-delivers. An empty
			// SenderID is a separate error wireRoutes reports ("unknown sender").
			if bd.SenderID == "" {
				continue
			}
			if prev, seen := dedicatedSender[bd.SessionID]; seen {
				if prev != bd.SenderID {
					return fmt.Errorf("bridge: route %q: binding %q drains dedicated session %q with "+
						"sender %q, but that session is already drained by sender %q; a session has a "+
						"single outbox drainer, so one binding's records would be delivered to the wrong "+
						"destination — give each sender its own session_id",
						rd.ID, bd.ID, bd.SessionID, bd.SenderID, prev)
				}
				continue
			}
			dedicatedSender[bd.SessionID] = bd.SenderID
		}
	}
	return nil
}
func (b *Builder) buildSessionsWithURIs(ctx context.Context) (map[string]ports.Session, map[string]string, error) {
	sessions := make(map[string]ports.Session, len(b.cfg.Sessions))
	uris := make(map[string]string, len(b.cfg.Sessions))

	// A SessionDef is only worth constructing when something references it: a
	// route's primary session, a binding, a receiver, or a sender. An
	// unreferenced session gets no session manager and is never handed to the
	// runtime, so it would open a connection/lease that Stop never closes —
	// a leak on every hot-reload (Finding 6). Skip them with a warning.
	referenced := referencedSessionIDs(b.cfg)

	cleanup := func(exclude string) {
		for id, s := range sessions {
			if id == exclude {
				continue
			}
			if closeErr := s.Close(ctx); closeErr != nil && b.logger != nil {
				b.logger.Warn("closing session after partial failure", "session", id, "error", closeErr)
			}
		}
	}

	for _, sd := range b.cfg.Sessions {
		if !referenced[sd.ID] {
			if b.logger != nil {
				b.logger.Warn("bridge: skipping unreferenced session; no route, binding, receiver, "+
					"or sender references it so it would never be managed or closed",
					"session", sd.ID, "transport", sd.Transport)
			}
			continue
		}
		tf, ok := b.transports[sd.Transport]
		if !ok {
			cleanup("")
			return nil, nil, fmt.Errorf("bridge: no transport factory registered for %q (session %q)", sd.Transport, sd.ID)
		}
		uri, err := b.resolveConfigCredentials(ctx, sd.Config, fmt.Sprintf("session %q", sd.ID))
		if err != nil {
			cleanup("")
			return nil, nil, err
		}
		if uri != "" {
			uris[sd.ID] = uri
		}
		sess, err := tf.NewSession(ctx, sessionSpecFrom(sd))
		if err != nil {
			cleanup("")
			return nil, nil, fmt.Errorf("bridge: create session %q: %w", sd.ID, err)
		}
		if sess != nil {
			sessions[sd.ID] = sess
		}
	}
	return sessions, uris, nil
}

// referencedSessionIDs returns the set of session IDs referenced by any route
// primary session, binding, receiver, or sender. Used to avoid constructing
// (and leaking) unreferenced SessionDefs (Finding 6).
func referencedSessionIDs(cfg *ports.BridgeConfig) map[string]bool {
	ref := make(map[string]bool, len(cfg.Sessions))
	for i := range cfg.Routes {
		rd := &cfg.Routes[i]
		if rd.Session != nil && rd.Session.SessionID != "" {
			ref[rd.Session.SessionID] = true
		}
		for _, bd := range toBindings(cfg, rd.Bindings) {
			if bd.SessionID != "" {
				ref[bd.SessionID] = true
			}
		}
	}
	for i := range cfg.Receivers {
		if cfg.Receivers[i].SessionID != "" {
			ref[cfg.Receivers[i].SessionID] = true
		}
	}
	for i := range cfg.Senders {
		if cfg.Senders[i].SessionID != "" {
			ref[cfg.Senders[i].SessionID] = true
		}
	}
	return ref
}

// buildReceiversWithURIs mirrors buildSessionsWithURIs: it returns the
// receiver-level credentials_uri (captured BEFORE resolveCredentials
// removes it) so CredentialRefresher can bind watchers per receiver.
func (b *Builder) buildReceiversWithURIs(ctx context.Context, sessions map[string]ports.Session) (map[string]ports.Receiver, map[string]string, error) {
	receivers := make(map[string]ports.Receiver, len(b.cfg.Receivers))
	uris := make(map[string]string, len(b.cfg.Receivers))
	for _, rd := range b.cfg.Receivers {
		transport := rd.Transport
		if transport == "" {
			if sd := findSession(b.cfg, rd.SessionID); sd != nil {
				transport = sd.Transport
			}
		}
		tf, ok := b.transports[transport]
		if !ok {
			return nil, nil, fmt.Errorf("bridge: no transport factory for %q (receiver %q)", transport, rd.ID)
		}
		var sess ports.Session
		if rd.SessionID != "" {
			sess = sessions[rd.SessionID]
			if sess == nil {
				// Distinguish a genuinely undeclared session from one that is
				// declared but resolved to a nil session because its transport
				// is stateless (NewSession returns nil). The old code reported
				// both as "references unknown session", which misled operators
				// debugging a stateless-transport session (Finding 10).
				if sd := findSession(b.cfg, rd.SessionID); sd != nil {
					return nil, nil, fmt.Errorf("bridge: receiver %q references session %q whose "+
						"transport %q is stateless (it creates no session object); a receiver cannot "+
						"bind to a stateless session — remove session_id, or point it at a session on a "+
						"stateful transport", rd.ID, rd.SessionID, sd.Transport)
				}
				return nil, nil, fmt.Errorf("bridge: receiver %q references unknown session %q", rd.ID, rd.SessionID)
			}
		}
		uri, err := b.resolveConfigCredentials(ctx, rd.Config, fmt.Sprintf("receiver %q", rd.ID))
		if err != nil {
			return nil, nil, err
		}
		if uri != "" {
			uris[rd.ID] = uri
		}
		recv, err := tf.NewReceiver(ctx, receiverSpecFrom(rd), sess)
		if err != nil {
			return nil, nil, fmt.Errorf("bridge: create receiver %q: %w", rd.ID, err)
		}
		receivers[rd.ID] = recv
	}
	return receivers, uris, nil
}

// buildSendersWithURIs parallels buildReceiversWithURIs.
func (b *Builder) buildSendersWithURIs(ctx context.Context, sessions map[string]ports.Session) (map[string]ports.Sender, map[string]string, error) {
	senders := make(map[string]ports.Sender, len(b.cfg.Senders))
	uris := make(map[string]string, len(b.cfg.Senders))
	for _, sd := range b.cfg.Senders {
		transport := sd.Transport
		if transport == "" {
			if sess := findSession(b.cfg, sd.SessionID); sess != nil {
				transport = sess.Transport
			}
		}
		tf, ok := b.transports[transport]
		if !ok {
			return nil, nil, fmt.Errorf("bridge: no transport factory for %q (sender %q)", transport, sd.ID)
		}
		var sess ports.Session
		if sd.SessionID != "" {
			sess = sessions[sd.SessionID]
			if sess == nil {
				// See buildReceiversWithURIs: distinguish undeclared from
				// declared-but-stateless (Finding 10).
				if decl := findSession(b.cfg, sd.SessionID); decl != nil {
					return nil, nil, fmt.Errorf("bridge: sender %q references session %q whose "+
						"transport %q is stateless (it creates no session object); a sender cannot "+
						"bind to a stateless session — remove session_id, or point it at a session on a "+
						"stateful transport", sd.ID, sd.SessionID, decl.Transport)
				}
				return nil, nil, fmt.Errorf("bridge: sender %q references unknown session %q", sd.ID, sd.SessionID)
			}
		}
		uri, err := b.resolveConfigCredentials(ctx, sd.Config, fmt.Sprintf("sender %q", sd.ID))
		if err != nil {
			return nil, nil, err
		}
		if uri != "" {
			uris[sd.ID] = uri
		}
		snd, err := tf.NewSender(ctx, senderSpecFrom(sd), sess)
		if err != nil {
			return nil, nil, fmt.Errorf("bridge: create sender %q: %w", sd.ID, err)
		}
		senders[sd.ID] = snd
	}
	return senders, uris, nil
}

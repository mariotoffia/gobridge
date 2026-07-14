package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// builderCloseBudget bounds the best-effort teardown of receivers/senders (and
// their network clients / broker links) when complete() fails after building
// them. It is detached from the possibly-already-expired build ctx so a
// deadline-expired swap still releases the links (HIGH-5).
const builderCloseBudget = 5 * time.Second

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

	sessions, sessionURIs, err := b.buildSessionsWithURIs(ctx, prep.stores.managedSubscriptions)
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

	receivers, receiverURIs, err := b.buildReceiversWithURIs(ctx, sessions)
	if err != nil {
		return nil, err
	}
	// Receivers may hold network clients / broker links (Service Bus, AMQP 1.0,
	// HTTP SSE). A LATER failure in this phase (sender build, wireRoutes, or
	// ValidateRoutes) would otherwise leak every one on each failed reload — the
	// runtime that would own and Stop them is never returned. Close any that
	// implement ports.ContextCloser on every failure path, mirroring the
	// session/store defers (HIGH-5). Registered after the session defer so it
	// runs BEFORE it (LIFO): tear the links down before their sessions.
	defer func() {
		if retErr != nil {
			closeBuiltContextClosers(ctx, b.logger, "receiver", receivers)
		}
	}()

	senders, senderURIs, err := b.buildSendersWithURIs(ctx, sessions)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			closeBuiltContextClosers(ctx, b.logger, "sender", senders)
		}
	}()

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
		// Emit MetricCredentialRotationApplied per applied rotation so the
		// success side of credential rotation is observable (F4).
		if b.metrics != nil {
			refresherOpts = append(refresherOpts, WithRefresherMetrics(b.metrics))
		}
		// Wire push rotations to the pull-cache invalidator so the next
		// synchronous resolve fetches fresh material (contract C4) — but ONLY for
		// a decoupled push store. The coherent lazy-wrapper path already refreshes
		// this same resolver's cache on the detecting poll, so invalidating there
		// would delete a just-cached fresh entry and blind F5 stale-serve for a
		// poll interval (see pullCacheNeedsRotationInvalidation / adversarial
		// Finding 1). Detect the capability structurally to avoid importing runtime.
		if b.pullCacheNeedsRotationInvalidation() {
			if inv, ok := b.credStore.(interface{ InvalidateCache(uri string) }); ok {
				refresherOpts = append(refresherOpts, WithRotationCallback(inv.InvalidateCache))
			}
		}
		refresher := NewCredentialRefresher(pushStore, b.logger, refresherOpts...)
		// The refresher's Watch* calls start poll goroutines immediately. On the
		// SUCCESS path the runtime takes ownership via AttachCredentialCloser
		// below and closes it on Stop. But if a LATER step fails (e.g.
		// ValidateRoutes returns nil runtime), that closer never runs and the
		// poll goroutines leak — hammering the credential source and writing into
		// just-closed sessions. Mirror the session/store failure defers: close the
		// refresher when complete() returns an error (HIGH: refresher leak on
		// ValidateRoutes failure).
		defer func() {
			if retErr != nil {
				refresher.Close()
			}
		}()
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

// closeBuiltContextClosers closes any built receivers or senders that implement
// ports.ContextCloser when complete() fails after they were constructed. Several
// production adapters (Service Bus receiver/sender, AMQP 1.0 receiver/sender,
// HTTP SSE sender) hold network clients or broker links that would otherwise
// leak on every failed reload, since the runtime that owns and Stops them is
// never returned (HIGH-5). Teardown is best-effort and bounded by a fresh
// budget DETACHED from ctx: complete() often fails BECAUSE ctx expired, and a
// dead ctx would make every Close return immediately without releasing the link.
func closeBuiltContextClosers[T any](ctx context.Context, logger *slog.Logger, kind string, items map[string]T) {
	if len(items) == 0 {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), builderCloseBudget)
	defer cancel()
	for id, item := range items {
		cc, ok := any(item).(ports.ContextCloser)
		if !ok {
			continue
		}
		if err := cc.Close(closeCtx); err != nil && logger != nil {
			logger.Warn("closing "+kind+" after build failure", "id", id, "error", err)
		}
	}
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
	for _, s := range []any{stores.managedSubscriptions, stores.outbox, stores.dlq, stores.lease} {
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
	clustered := deploymentClustered(b.cfg)
	if routeDef.Session != nil {
		derived, err := toSessionConfigE(routeDef.Session, clustered)
		if err != nil {
			return session.Config{}, err
		}
		sc := *derived
		sc.SessionID = sessionID
		applyBridgeDrainDefaults(&sc, b.cfg.Bridge)
		return sc, nil
	}
	// No inline session block: this binding-only exclusive sender inherits the
	// same clustered HA-timing default as the rest of the deployment (HIGH-3),
	// so a clustered failover for it also lands in the 30-60s band. Non-clustered
	// keeps DefaultConfig with RenewInterval reset so the manager derives it.
	if clustered {
		sc := session.HAConfig(sessionID, true)
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
	// slowFailoverWarned dedupes the F-1 advisory per session so a session
	// shared by several routes is flagged at most once.
	slowFailoverWarned := make(map[string]bool)

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
		sessCfg, sessCfgErr := toSessionConfigE(routeDef.Session, deploymentClustered(b.cfg))
		if sessCfgErr != nil {
			return fmt.Errorf("bridge: route %q: %w", routeDef.ID, sessCfgErr)
		}
		applyBridgeDrainDefaults(sessCfg, b.cfg.Bridge)
		b.warnSlowClusterFailover(routeDef, sessCfg, slowFailoverWarned)

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
		var sourceTransport string

		recvDef := findReceiver(b.cfg, routeDef.ReceiverID)
		if recvDef != nil {
			transport := recvDef.Transport
			if transport == "" {
				if sd := findSession(b.cfg, recvDef.SessionID); sd != nil {
					transport = sd.Transport
				}
			}
			// Record the resolved source transport identity so the runtime can
			// strip foreign redelivery-count headers on ingress (F3). Prefer the
			// receiver config's canonical Kind() (e.g. "aws.sqs") over the
			// operator-chosen registry name: a count-bearing transport registered
			// under a custom name would otherwise have its OWN redelivery-count
			// header stripped as foreign, silently disabling the replay cap. Falls
			// back to the registry name when the receiver carries no typed plugin
			// config (count-less transports, which strip all count headers anyway).
			sourceTransport = transport
			if recvDef.Config != nil {
				if k := recvDef.Config.Kind(); k != "" {
					sourceTransport = k
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
				// A per-route receiver config may also narrow the source
				// capabilities below the transport-wide Factory constant when
				// the receiver's MODE implements a smaller set (e.g. ASB
				// ReceiveAndDelete cannot redeliver, so it drops
				// CapVisibilityExtension/CapSourceRedelivery). The validator's
				// silent-drop check then sees the honest per-route set instead
				// of the transport-wide constant (C14 F4).
				if cc, ok := recvDef.Config.(ports.CapabilityConfig); ok {
					caps = cc.Capabilities()
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

		// F-3: warn when a route's egress can lose an accepted publish at crash
		// in a way that would become bridge-level loss. See
		// egressDurabilityAdvisory — silent for both current delivery modes.
		if b.logger != nil &&
			egressDurabilityAdvisory(routing.DeliveryMode(routeDef.DeliveryMode), routeSender) {
			b.logger.Warn("bridge: route egress is non-durable and its delivery mode acks the source "+
				"before the egress durability boundary — an accepted publish lost at process crash becomes "+
				"bridge-level message loss; use shared_outbox (durable persist) or a delivery mode that acks "+
				"the source only after the transport confirms",
				"route", routeDef.ID,
				"delivery_mode", routeDef.DeliveryMode,
			)
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
			SourceTransport:         sourceTransport,
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

// failoverBandMaxTTL is the upper bound of the documented clustered-failover
// band (30-60s). A clustered exclusive session whose lease TTL exceeds it will
// not have a dead owner's partition reclaimed by a peer for that whole TTL, so
// it misses the stated failover requirement (F-1).
const failoverBandMaxTTL = 60 * time.Second

// warnSlowClusterFailover emits the F-1 advisory once per session when a
// CLUSTERED deployment runs an exclusive (lease-bearing) session whose effective
// lease TTL is slower than the documented 30-60s failover band.
//
// Clustered sessions that do not pin lease timing already default to the 45s HA
// profile (toSessionConfigE), which is in band and does NOT warn. This fires
// only when the operator EXPLICITLY pinned a loose lease_ttl on a cluster,
// making a slow failover a deliberate, visible choice rather than a silent
// surprise. It is scoped to clustered deployments because failover — a peer
// reclaiming a dead owner's partition — is only meaningful with a peer; a
// single-node deployment has none, so a loose TTL there is not warned.
func (b *Builder) warnSlowClusterFailover(
	routeDef ports.RouteDef,
	sessCfg *session.Config,
	warned map[string]bool,
) {
	// sessCfg is non-nil only for a RouteSessionDef source, which is always an
	// exclusive single-owner lease session — so a non-nil sessCfg already
	// implies "exclusive".
	if b.logger == nil || sessCfg == nil || routeDef.Session == nil {
		return
	}
	if !deploymentClustered(b.cfg) || sessCfg.LeaseTTL <= failoverBandMaxTTL {
		return
	}
	sid := routeDef.Session.SessionID
	if warned[sid] {
		return
	}
	warned[sid] = true
	b.logger.Warn("bridge: clustered exclusive session has a lease TTL slower than the documented "+
		"30-60s failover band — if the owning node dies, its partition is not reclaimed by a peer for "+
		"the whole TTL; pin a lease_ttl within the band or omit lease timing to use the 45s HA default",
		"session", sid,
		"lease_ttl", sessCfg.LeaseTTL,
		"failover_band_max", failoverBandMaxTTL,
	)
}

// egressDurabilityAdvisory reports whether a route wiring warrants an
// egress-durability WARN. Two conditions must both hold:
//
//  1. the route's Sender declares NON-DURABLE egress via
//     ports.NonDurableEgressReporter — its accepted-but-in-flight packet state
//     can be lost at process crash (MQTT autopaho at QoS 1/2); AND
//  2. the delivery mode acknowledges the SOURCE before that egress durability
//     boundary, so the lost publish is never redelivered nor replayed and the
//     transport loss becomes BRIDGE-LEVEL message loss.
//
// A Sender that does not implement the reporter interface is treated as
// durable-egress (no advisory).
//
// ponytail: this is a forward-guard and is SILENT for both current delivery
// modes. direct_hold acks the source only after the broker PUBACK/PUBCOMP
// (runtime/route/dispatch.go), and shared_outbox only after a version-fenced
// outbox persist (runtime/validator.go), so neither acks the source ahead of
// the egress durability boundary — a non-durable egress causes no bridge-level
// loss on either. The advisory therefore fires today for nothing, by design; it
// exists so that a future delivery mode which acks the source early trips a
// startup WARN instead of losing messages silently in production. It inspects
// the route's PRIMARY sender (the documented single-dispatch path); extending it
// to per-binding fan-out senders is a trivial follow-up if such a mode is ever
// added.
//
// An unset (empty) mode is normalised to the documented default (direct_hold),
// matching how routing.RoutePolicy.WithDefaults resolves an omitted
// delivery_mode, so a route that omits the field is treated as the loss-safe
// default and stays silent. A non-empty UNRECOGNISED mode is left to trip the
// default branch: config.Validate (validate.ValidateBlueprintGraph → the
// delivery_mode validateEnum) rejects a typo up front, so the only non-empty
// value that reaches the default branch is a genuinely NEW mode a future author
// added to DeliveryMode without special-casing it here.
func egressDurabilityAdvisory(mode routing.DeliveryMode, snd ports.Sender) bool {
	reporter, ok := snd.(ports.NonDurableEgressReporter)
	if !ok || !reporter.NonDurableEgress() {
		return false
	}
	if mode == "" {
		mode = routing.DeliveryDirectHold
	}
	switch mode {
	case routing.DeliveryDirectHold, routing.DeliverySharedOutbox:
		// Source ack is gated behind the egress durability boundary.
		return false
	default:
		return true
	}
}

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
func (b *Builder) buildSessionsWithURIs(ctx context.Context, managedStore ports.ManagedSubscriptionStore) (map[string]ports.Session, map[string]string, error) {
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
		spec, err := sessionSpecWithManagedSubscriptions(sd, b.cfg, managedStore)
		if err != nil {
			cleanup("")
			return nil, nil, fmt.Errorf("bridge: create session spec %q: %w", sd.ID, err)
		}
		sess, err := tf.NewSession(ctx, spec)
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
func (b *Builder) buildReceiversWithURIs(ctx context.Context, sessions map[string]ports.Session) (_ map[string]ports.Receiver, _ map[string]string, retErr error) {
	receivers := make(map[string]ports.Receiver, len(b.cfg.Receivers))
	uris := make(map[string]string, len(b.cfg.Receivers))
	// A mid-loop failure (a LATER receiver's NewReceiver erroring) returns
	// (nil, nil, err), so complete's own receiver defer — which closes the
	// RETURN value — sees nil and cannot release the receivers already built
	// this pass. They may hold broker links (Service Bus, AMQP 1.0, HTTP SSE),
	// so close the partial LOCAL map here (HIGH-5). The map returns stay blank
	// (_) so `return nil, nil, err` does not nil out the map this defer reads.
	// This is complementary to complete's defer, never a double close: a failed
	// pass returns before complete registers its receiver defer, and a fully
	// built pass leaves retErr nil here so only complete's defer runs.
	defer func() {
		if retErr != nil {
			closeBuiltContextClosers(ctx, b.logger, "receiver", receivers)
		}
	}()
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
func (b *Builder) buildSendersWithURIs(ctx context.Context, sessions map[string]ports.Session) (_ map[string]ports.Sender, _ map[string]string, retErr error) {
	senders := make(map[string]ports.Sender, len(b.cfg.Senders))
	uris := make(map[string]string, len(b.cfg.Senders))
	// See buildReceiversWithURIs: close the partial LOCAL map on a mid-loop
	// failure so senders already built this pass (which may hold broker links)
	// are not leaked past complete's return-value-scoped defer (HIGH-5).
	defer func() {
		if retErr != nil {
			closeBuiltContextClosers(ctx, b.logger, "sender", senders)
		}
	}()
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

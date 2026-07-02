package bridge

import (
	"context"
	"fmt"
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
	// CredentialAware. Gated on pushCredStore so builds without a push
	// store skip this entirely, preserving legacy behavior.
	if b.pushCredStore != nil && (len(sessionURIs)+len(receiverURIs)+len(senderURIs)) > 0 {
		refresher := NewCredentialRefresher(b.pushCredStore, b.logger)
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

	return rt, nil
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
			sessCfg.Plan = sessionPlanFor(b.cfg, routeDef.Session.SessionID)
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
				return fmt.Errorf("bridge: route %q: binding %q references unknown session %q", routeDef.ID, bd.ID, bd.SessionID)
			}
			snd, sndOk := senders[bd.SenderID]
			if !sndOk {
				return fmt.Errorf("bridge: route %q: binding %q references unknown sender %q", routeDef.ID, bd.ID, bd.SenderID)
			}
			sc := session.DefaultConfig(bd.SessionID, true)
			sc.ConnectAfterLease = true
			// Thread the session's desired topology so a session registered only
			// via a binding (Path-2) still reconciles its receivers' subscriptions
			// and sender exchanges instead of an empty plan (F1-P4). Mirrors the
			// Path-1 assignment above. bd.SessionID is non-empty (guarded above).
			sc.Plan = sessionPlanFor(b.cfg, bd.SessionID)
			if err := rt.RegisterSessionSender(sc, sess, snd); err != nil {
				return fmt.Errorf("bridge: register session sender %q: %w", bd.SessionID, err)
			}
			registeredSessions[bd.SessionID] = true
		}
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

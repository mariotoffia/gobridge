package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Complete creates sessions, receivers, senders, wires routes, and
// returns a ready-to-start Runtime. Call after the old runtime has
// released exclusive resources (e.g. MQTT client-ids). If prep is
// nil, Complete returns an error.
func (b *Builder) Complete(ctx context.Context, prep *PreparedBuild) (_ *runtime.Runtime, retErr error) {
	if prep == nil {
		return nil, fmt.Errorf("bridge: Complete called with nil PreparedBuild")
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
		runtime.AttachCredentialCloser(rt, refresher.Close)
	}

	return rt, nil
}

func (b *Builder) wireRoutes(
	rt *runtime.Runtime,
	sessions map[string]ports.Session,
	receivers map[string]ports.Receiver,
	senders map[string]ports.Sender,
) error {
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

		var routeSession ports.Session
		var routeSender ports.Sender
		var caps []ports.Capability
		var sourceVisTimeout time.Duration

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
			sc := runtime.DefaultSessionConfig(bd.SessionID, true)
			sc.ConnectAfterLease = true
			if err := rt.RegisterSessionSender(sc, sess, snd); err != nil {
				return fmt.Errorf("bridge: register session sender %q: %w", bd.SessionID, err)
			}
			registeredSessions[bd.SessionID] = true
		}
	}

	return nil
}

// session-id → credentials_uri map. The URI is captured BEFORE
// resolveCredentials consumes and deletes it, so the credential
// refresher can bind watchers after session construction.
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
		if sd.Options != nil {
			if u, ok := sd.Options["credentials_uri"].(string); ok && u != "" {
				uris[sd.ID] = u
			}
			opts, err := b.resolveCredentials(ctx, sd.Options, fmt.Sprintf("session %q", sd.ID))
			if err != nil {
				cleanup("")
				return nil, nil, err
			}
			sd.Options = opts
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
		if rd.Options != nil {
			if u, ok := rd.Options["credentials_uri"].(string); ok && u != "" {
				uris[rd.ID] = u
			}
			opts, err := b.resolveCredentials(ctx, rd.Options, fmt.Sprintf("receiver %q", rd.ID))
			if err != nil {
				return nil, nil, err
			}
			rd.Options = opts
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
		if sd.Options != nil {
			if u, ok := sd.Options["credentials_uri"].(string); ok && u != "" {
				uris[sd.ID] = u
			}
			opts, err := b.resolveCredentials(ctx, sd.Options, fmt.Sprintf("sender %q", sd.ID))
			if err != nil {
				return nil, nil, err
			}
			sd.Options = opts
		}
		snd, err := tf.NewSender(ctx, senderSpecFrom(sd), sess)
		if err != nil {
			return nil, nil, fmt.Errorf("bridge: create sender %q: %w", sd.ID, err)
		}
		senders[sd.ID] = snd
	}
	return senders, uris, nil
}

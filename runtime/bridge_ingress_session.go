package runtime

import (
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// An ingress session is a session whose only role is to carry the
// subscriptions of the receivers bound to it: no route names it in a session
// block and no binding drains an outbox partition through it. A plan-driven
// transport (MQTT, AMQP 0-9-1) subscribes only when a session manager
// reconciles the session plan, so such a session still needs a manager — one
// that starts the session, reconciles its plan and follows reconnects. It holds
// no lease: there is no partition to fence and no ownership to arbitrate, which
// is exactly the shape a direct_hold route holds its source in.

// ingressSessionEntry pairs an ingress session with the manager configuration
// it runs under.
type ingressSessionEntry struct {
	config  session.Config
	session ports.Session
}

// RegisterIngressSession registers a session for the receivers that subscribe
// through it. The runtime gives it a plain session manager at Start and the
// same ingress settlement barrier a route-primary session gets before it
// recycles a broker connection.
//
// A session has exactly one manager, so an id that is already a session sender
// or a route's primary session is refused here rather than left for Start to
// choose between. The configuration may not be exclusive: an ingress session
// holds no lease by definition, and a lease-bearing session is registered
// through a route session block or RegisterSessionSender instead.
func (rt *Runtime) RegisterIngressSession(cfg session.Config, sess ports.Session) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.running {
		return errors.New("cannot register ingress session on running runtime")
	}
	if cfg.SessionID == "" {
		return errors.New("session ID is required")
	}
	if sess == nil {
		return fmt.Errorf("ingress session %q: session is required", cfg.SessionID)
	}
	if cfg.Exclusive {
		return fmt.Errorf("ingress session %q: an ingress session holds no lease; register a lease-bearing "+
			"session through a route session block or RegisterSessionSender", cfg.SessionID)
	}
	if _, exists := rt.ingressSessions[cfg.SessionID]; exists {
		return errors.New("duplicate ingress session: " + cfg.SessionID)
	}
	if _, exists := rt.sessionSenders[cfg.SessionID]; exists {
		return fmt.Errorf("ingress session %q is already registered as a session sender; a session has one manager",
			cfg.SessionID)
	}
	if routeID, primary := rt.routePrimarySessionLocked(cfg.SessionID); primary {
		return fmt.Errorf("ingress session %q is already the primary session of route %q; a session has one manager",
			cfg.SessionID, routeID)
	}

	rt.ingressSessions[cfg.SessionID] = &ingressSessionEntry{config: cfg, session: sess}
	return nil
}

// routePrimarySessionLocked reports whether a registered route names sessionID
// in its session block, and which route. The caller holds rt.mu.
func (rt *Runtime) routePrimarySessionLocked(sessionID string) (string, bool) {
	for _, entry := range rt.entries {
		if entry.sessCfg != nil && entry.sessCfg.SessionID == sessionID {
			return entry.config.ID, true
		}
	}
	return "", false
}

// attachIngressSessions gives every ingress session its manager and enrols it
// in the settlement barrier for the routes whose receivers ride on it. It runs
// under rt.mu during Start, after the route-primary and session-sender
// managers exist, so a lease-bearing manager would always win — although
// registration already refuses that overlap.
func (rt *Runtime) attachIngressSessions(
	m ports.MetricsExporter,
	settlementSessions map[string]ports.Session,
	settlementRoutes map[string][]string,
) {
	for sid, entry := range rt.ingressSessions {
		if _, exists := rt.sessionMgrs[sid]; !exists {
			mgr := session.NewWithMetrics(entry.config, entry.session, rt.leaseStore, rt.leaseOwnerID, rt.logger, m, rt.clk)
			mgr.SetAudit(rt.audit)
			mgr.SetEndpoints(rt.clusterEndpoints)
			rt.sessionMgrs[sid] = mgr
		}
		// A route rides on this session when its receiver subscribes through it
		// (the builder says which), or — for a hand-wired runtime — when the
		// session it was added with is this one and it names no primary of its
		// own. A route whose primary session is a DIFFERENT, lease-held session
		// still rides its receiver on this one and still needs the barrier.
		for _, route := range rt.entries {
			ridesOn := route.config.SourceSessionID == sid ||
				(route.sessCfg == nil && route.session == entry.session)
			if !ridesOn {
				continue
			}
			settlementSessions[sid] = entry.session
			settlementRoutes[sid] = append(settlementRoutes[sid], route.config.ID)
		}
	}
}

// sessionRef is one session the runtime was handed, with the id it is managed
// under when it is managed at all.
type sessionRef struct {
	sid  string
	sess ports.Session
}

// unmanagedSessionRefsLocked returns every session the runtime was handed that
// no manager owns, so Stop can close it. managed is the set of session ids that
// have a manager. A session is matched by id where the entry carries one and by
// pointer otherwise: a route entry that only rides on a binding-managed or
// ingress session names no id of its own, and treating it as unmanaged would
// close a session its manager has already closed. The caller holds rt.mu.
func (rt *Runtime) unmanagedSessionRefsLocked(managed map[string]bool) []sessionRef {
	managedSessions := make(map[ports.Session]bool, len(managed))
	for _, entry := range rt.entries {
		if entry.sessCfg != nil && entry.session != nil && managed[entry.sessCfg.SessionID] {
			managedSessions[entry.session] = true
		}
	}
	for sid, sse := range rt.sessionSenders {
		if managed[sid] {
			managedSessions[sse.session] = true
		}
	}
	for sid, ise := range rt.ingressSessions {
		if managed[sid] {
			managedSessions[ise.session] = true
		}
	}

	refs := make([]sessionRef, 0, len(rt.entries)+len(rt.sessionSenders)+len(rt.ingressSessions))
	seen := make(map[ports.Session]bool)
	add := func(sid string, sess ports.Session) {
		if sess == nil || managed[sid] || managedSessions[sess] || seen[sess] {
			return
		}
		seen[sess] = true
		refs = append(refs, sessionRef{sid: sid, sess: sess})
	}
	for _, entry := range rt.entries {
		sid := ""
		if entry.sessCfg != nil {
			sid = entry.sessCfg.SessionID
		}
		add(sid, entry.session)
	}
	for sid, sse := range rt.sessionSenders {
		add(sid, sse.session)
	}
	for sid, ise := range rt.ingressSessions {
		add(sid, ise.session)
	}
	return refs
}

package runtime

import (
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// managerSource is the registration a session's one manager is built from.
type managerSource struct {
	config session.Config
	// session is the session object the manager runs. Two registrations of
	// one session id may carry different objects; only this one is started.
	session ports.Session
	// routeID is the route whose session block it is; "" for a session sender
	// or an ingress session.
	routeID string
}

// managerSources resolves, for every session id s registers, the registration
// its manager is built from, without building anything. A session has exactly
// one manager (ensureSessionManagerLocked), built from the first registration
// startComponentsLocked reaches: the routes in order, each with its own session
// block and then, on a shared_outbox route in a runtime with an outbox store,
// the session senders its bindings name; then the remaining session senders;
// then the ingress sessions. Every question about a session's lease (is it
// lease-managed, does it defer its connect, which object is it) is answered
// from this registration, or from the manager itself once it exists, never from
// another registration of the same session id.
func (s componentSet) managerSources(hasOutboxStore bool) map[string]managerSource {
	sources := make(map[string]managerSource)
	add := func(sid string, src managerSource) {
		if _, ok := sources[sid]; !ok {
			sources[sid] = src
		}
	}
	for _, entry := range s.entries {
		if entry.session != nil && entry.sessCfg != nil {
			add(entry.sessCfg.SessionID, managerSource{config: *entry.sessCfg, session: entry.session, routeID: entry.config.ID})
		}
		if !hasOutboxStore || entry.config.Policy.DeliveryMode != routing.DeliverySharedOutbox {
			continue
		}
		for _, b := range entry.config.Bindings {
			sid := b.SessionID
			if sid == "" && entry.sessCfg != nil {
				sid = entry.sessCfg.SessionID // the binding inherits the route's session block
			}
			if sse, ok := s.sessionSenders[sid]; ok {
				add(sid, managerSource{config: sse.config, session: sse.session})
			}
		}
	}
	for sid, sse := range s.sessionSenders {
		add(sid, managerSource{config: sse.config, session: sse.session})
	}
	for sid, ise := range s.ingressSessions {
		add(sid, managerSource{config: ise.config, session: ise.session})
	}
	return sources
}

// managerSourcesLocked resolves managerSources over everything registered in
// rt. Caller holds rt.mu.
func (rt *Runtime) managerSourcesLocked() map[string]managerSource {
	set := componentSet{entries: rt.entries, sessionSenders: rt.sessionSenders, ingressSessions: rt.ingressSessions}
	return set.managerSources(rt.outboxStore != nil)
}

// leaseManaged reports what session.Manager.Exclusive reports for a manager
// built from this registration: exclusive, in a runtime with a lease store.
func (src managerSource) leaseManaged(hasLeaseStore bool) bool {
	return src.config.Exclusive && hasLeaseStore
}

// defersConnect reports what session.Manager.DefersConnect reports for a
// manager built from this registration.
func (src managerSource) defersConnect(hasLeaseStore bool) bool {
	return src.leaseManaged(hasLeaseStore) && src.config.ConnectAfterLease
}

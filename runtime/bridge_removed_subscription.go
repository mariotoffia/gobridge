package runtime

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// installRemovedSubscriptionDeadLetter gives every managed session that supports
// it a dead-letter path for deliveries the broker still hands it for a
// subscription it removed. The session writes such a delivery here before
// acknowledging it, recorded as SUBSCRIPTION_REMOVED against the removed filter.
// A session with no route riding on it still gets the path: its plan may just
// have become empty. Without a dead-letter store nothing is installed, and the
// session keeps such a delivery unacknowledged, because acknowledging it
// without a durable copy would lose it.
func (rt *Runtime) installRemovedSubscriptionDeadLetter(dlqRouter *dlq.Router) {
	if !dlqRouter.HasStore() {
		return
	}
	for sid := range rt.sessionMgrs {
		configurer, ok := rt.managedSession(sid).(ports.RemovedSubscriptionDeadLetterConfigurer)
		if !ok {
			continue
		}
		sessionID := sid
		routeID := rt.sourceRouteOn(sid)
		configurer.SetRemovedSubscriptionDeadLetter(func(ctx context.Context, env *messaging.Envelope, filter string) error {
			// The session is also the source: the entry identity (envelope,
			// route, binding, source) must be scoped to the session, because a
			// routeless record otherwise shares one scope across sessions and
			// two sessions' deliveries with the same producer message ID would
			// collapse into one record, acknowledging the second without its
			// own copy. Envelope IDs are unique within a source, so the filter
			// is not needed in the identity.
			return dlqRouter.Route(ctx, env, routeID, "", filter, sessionID, sessionID, shared.ErrSubscriptionRemoved, 0)
		})
	}
}

// sourceRouteOn names the one route whose receiver subscribes through session
// sid. With none, or several, a record written for that session names no route.
func (rt *Runtime) sourceRouteOn(sid string) string {
	routeID := ""
	for _, entry := range rt.entries {
		if entry.config.SourceSessionID != sid {
			continue
		}
		if routeID != "" {
			return ""
		}
		routeID = entry.config.ID
	}
	return routeID
}

// managedSession resolves the session a manager was built for. A route whose
// session block names sid but carries no session instance is skipped, so the
// session registered as a sender or ingress session is found instead.
func (rt *Runtime) managedSession(sid string) ports.Session {
	for _, entry := range rt.entries {
		if entry.session != nil && entry.sessCfg != nil && entry.sessCfg.SessionID == sid {
			return entry.session
		}
	}
	if sse, ok := rt.sessionSenders[sid]; ok {
		return sse.session
	}
	if ise, ok := rt.ingressSessions[sid]; ok {
		return ise.session
	}
	return nil
}

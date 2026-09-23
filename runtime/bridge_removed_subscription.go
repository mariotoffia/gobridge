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
func (rt *Runtime) installRemovedSubscriptionDeadLetter(dlqRouter *dlq.Router, settlementRoutes map[string][]string) {
	if !dlqRouter.HasStore() {
		return
	}
	for sid := range rt.sessionMgrs {
		configurer, ok := rt.managedSession(sid).(ports.RemovedSubscriptionDeadLetterConfigurer)
		if !ok {
			continue
		}
		sessionID := sid
		// An MQTT session carries at most one ingress route; with several, or
		// none, the record names no route.
		routeID := ""
		if routes := settlementRoutes[sid]; len(routes) == 1 {
			routeID = routes[0]
		}
		configurer.SetRemovedSubscriptionDeadLetter(func(ctx context.Context, env *messaging.Envelope, filter string) error {
			return dlqRouter.Route(ctx, env, routeID, "", filter, sessionID, "", shared.ErrSubscriptionRemoved, 0)
		})
	}
}

// managedSession resolves the session a manager was built for.
func (rt *Runtime) managedSession(sid string) ports.Session {
	for _, entry := range rt.entries {
		if entry.sessCfg != nil && entry.sessCfg.SessionID == sid {
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

package runtime

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// installRemovedSubscriptionDeadLetter gives every managed session of
// sessionIDs that supports it a dead-letter path for deliveries the broker still
// hands it for a subscription it removed. The session writes such a delivery
// here before acknowledging it, recorded as SUBSCRIPTION_REMOVED against the
// removed filter. A session with no route riding on it still gets the path: its
// plan may just have become empty. Without a dead-letter store nothing is
// installed, and the session keeps such a delivery unacknowledged, because
// acknowledging it without a durable copy would lose it. When the session names
// its managed subscription identity, the record is marked for automatic redrive
// with the session, the filter and that identity as its facts (ADR 0019), so
// adding the same subscription back redrives it. Each confirmed write, a
// suppressed duplicate included, counts one DLQEntries in category
// subscription_removed, tagged with the record's route when it names one. Caller
// holds rt.mu.
func (rt *Runtime) installRemovedSubscriptionDeadLetter(dlqRouter *dlq.Router, metrics ports.MetricsExporter, sessionIDs []string) {
	if !dlqRouter.HasStore() {
		return
	}
	for _, sid := range sessionIDs {
		sess := rt.managedSession(sid)
		configurer, ok := sess.(ports.RemovedSubscriptionDeadLetterConfigurer)
		if !ok {
			continue
		}
		identity := ""
		if r, ok := sess.(ports.ManagedSubscriptionIdentityReporter); ok {
			identity = r.ManagedSubscriptionIdentity()
		}
		sessionID := sid
		routeID := rt.sourceRouteOn(sid)
		configurer.SetRemovedSubscriptionDeadLetter(func(ctx context.Context, env *messaging.Envelope, filter string) error {
			var opts []dlq.EntryOption
			if identity != "" {
				opts = append(opts, dlq.AutoRedrive(map[string]string{
					routing.ExtraInfoSessionID:       sessionID,
					routing.ExtraInfoSubscription:    filter,
					routing.ExtraInfoManagedIdentity: identity,
				}))
			}
			// The session is also the source: the entry identity (envelope,
			// route, binding, source) must be scoped to the session, because a
			// routeless record otherwise shares one scope across sessions and
			// two sessions' deliveries with the same producer message ID would
			// collapse into one record, acknowledging the second without its
			// own copy. Envelope IDs are unique within a source, so the filter
			// is not needed in the identity.
			if err := dlqRouter.Route(ctx, env, routeID, "", filter, sessionID, sessionID, shared.ErrSubscriptionRemoved, 0, opts...); err != nil {
				return err
			}
			// The delivery never reached a route, so it has a category of its
			// own: the conservation law leaves it out, and it is not a send
			// failure. An empty route_id is left out, not sent as "".
			tags := []shared.Tag{{Key: shared.TagKeyCategory, Value: "subscription_removed"}}
			if routeID != "" {
				tags = append(tags, shared.Tag{Key: shared.TagKeyRouteID, Value: routeID})
			}
			metrics.Counter(shared.MetricDLQEntries, 1, tags...)
			return nil
		})
	}
}

// sourceRouteOn names the one route whose receiver subscribes through session
// sid. With none, or several, a record written for that session names no route.
// Only SourceSessionID is trusted, not the route's session argument: the builder
// passes an egress binding's session there, so an SQS-to-MQTT route would be
// named and a redrive would publish the removed filter's messages to its
// destination. An empty route only makes that redrive fail; the record stays.
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

// managedSession resolves the session object sid's manager runs (see
// managerSources). Caller holds rt.mu.
func (rt *Runtime) managedSession(sid string) ports.Session {
	return rt.managerSourcesLocked()[sid].session
}

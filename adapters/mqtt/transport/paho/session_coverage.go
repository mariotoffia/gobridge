package paho

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// orphanUnsubscribeTimeout bounds a single best-effort UNSUBSCRIBE issued
// for an orphan topic (a broker-retained subscription for a route removed
// from config). It is deliberately short: the ack-and-drop in the router
// already unblocked the in-flight window, so this converge step must not
// hang Close.
const orphanUnsubscribeTimeout = 10 * time.Second

// unsubscribeOrphan issues a best-effort UNSUBSCRIBE for a topic whose
// publishes matched no configured receiver filter after the router's
// startup grace window — a broker-retained subscription for a route
// removed from config. Because clean_start=false resumes the persistent
// session, the broker keeps such an orphan subscription and keeps
// delivering QoS 1/2 publishes for it; the router already acks-and-drops
// those to free the in-flight window, and this call converges the broker
// state so redelivery stops.
//
// MQTT exposes no subscription listing, so the orphan is identified only
// by its own post-grace publish and unsubscribed by that EXACT topic
// name. UNSUBSCRIBE matches the FILTER a subscription was created with,
// never a topic that filter covers, so this converges an exact-filter
// orphan and CANNOT converge a wildcard or shared one: the broker answers
// 0x11 ("no subscription existed") and keeps delivering. That outcome is
// reported rather than logged as a cleanup — only exact managed
// subscription history names such a filter, and a managed session
// converges it through reconcile instead (see reconcileManagedUnsubscribe).
// The surviving orphan's publishes keep being acked-and-dropped, so the
// stall cannot recur. The router dedups per topic, so this runs at most
// once per orphan topic per process.
//
// A concrete topic that a STILL-DESIRED subscription covers (an active
// broker subscription in s.activeSubs, or a filter in the current plan) is
// NEVER an orphan: its receiver has merely not registered its handler yet
// (registration follows Start→Reconcile→Run and can lag the grace window
// on a degraded start). Unsubscribing it here would silently kill a live
// route until the next reconcile/reconnect, so the UNSUBSCRIBE is skipped
// for covered topics. The router's ack-and-drop of the early publishes is
// unavoidable (the broker already acked them), but the subscription stays
// intact so later publishes are delivered once the handler registers.
//
// It is invoked from the router's own goroutine (never the paho publish
// callback), tracked by the router WaitGroup so Session.Close awaits it.
func (s *Session) unsubscribeOrphan(topic string) {
	ctx, cancel := context.WithTimeout(context.Background(), orphanUnsubscribeTimeout)
	defer cancel()
	// Use the session serialization boundary so an orphan decision and
	// destructive broker operation cannot interleave with a route re-add.
	if err := s.acquireReload(ctx); err != nil {
		return
	}
	defer s.releaseReload()

	// Recheck coverage only after owning reloadGate, immediately before the
	// broker operation. Reconcile holds the same gate while declaring its plan.
	s.mu.Lock()
	cm := s.cm
	closed := s.closed
	covered := s.topicCoveredLocked(topic)
	managed := s.managedRequired
	startEpoch := s.connEpoch
	s.mu.Unlock()
	if cm == nil || closed || managed {
		return
	}

	if covered {
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(context.Background(), logging.LevelDebug,
				"mqtt: skipping orphan unsubscribe; topic covered by a configured subscription (handler not yet registered)",
				"client_id", s.opts.ClientID,
				"topic", topic,
			)
		}
		return
	}

	removed := false
	reasons, err := cm.Unsubscribe(ctx, []string{topic})
	if err == nil {
		confirmation := classifyUnsubackReasons([]string{topic}, reasons)
		removed = confirmation.removedAny
		if confirmation.firstErr != nil {
			err = confirmation.firstErr
		} else if len(confirmation.confirmed) != 1 {
			err = shared.ErrProtocolError.WithMessage("mqtt: orphan UNSUBACK did not confirm removal")
		}
	}
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("mqtt: failed to unsubscribe orphan topic",
				"client_id", s.opts.ClientID,
				"topic", topic,
				"error", err,
			)
		}
		return
	}

	if !removed {
		// UNSUBACK 0x11: the broker holds no subscription under this exact
		// name, so the orphan was created with a wildcard or shared filter
		// this session cannot reconstruct from a delivered topic. Guessing a
		// filter would risk removing a LIVE subscription, so nothing is
		// removed and nothing is claimed. Ingress is unaffected (the publishes
		// are acked-and-dropped), but MetricMQTTRouterUnmatchedDropped will
		// keep rising for this filter until an operator removes it at the
		// broker or the session runs with managed subscriptions, whose exact
		// durable history converges it on the next reconcile.
		if s.logger != nil {
			s.logger.Warn("mqtt: orphan broker subscription survived cleanup; its filter is "+
				"wildcarded or shared and MQTT cannot unsubscribe it by a delivered topic — "+
				"enable managed subscriptions for exact durable filters, or remove it at the broker",
				"client_id", s.opts.ClientID,
				"topic", topic,
			)
		}
		return
	}

	s.mu.Lock()
	if s.connEpoch == startEpoch {
		delete(s.observedSubs, topic)
		delete(s.activeSubs, topic)
	}
	s.mu.Unlock()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: unsubscribed orphan topic",
			"client_id", s.opts.ClientID,
			"topic", topic,
		)
	}
}

// appliedPlanTopicsLocked returns the topic filters of the last SUCCESSFULLY
// reconciled plan (s.appliedPlan), or nil when no plan has been applied yet. It
// is the applied-state history Reconcile hands to s.reconcile so an empty
// target plan tears down the topics actually established on the broker — even
// when a reconnect has just reset the volatile activeSubs snapshot
// AND even when a prior reconcile FAILED to unsubscribe them
// (blocking-#2: the failed op's topics stay in the applied set until an
// unsubscribe succeeds). Callers must hold s.mu.
func (s *Session) appliedPlanTopicsLocked() []string {
	if s.appliedPlan == nil {
		return nil
	}
	topics := make([]string, 0, len(s.appliedPlan.Subscriptions))
	for _, sub := range s.appliedPlan.Subscriptions {
		topics = append(topics, sub.Topic)
	}
	return topics
}

// topicCoveredLocked reports whether a concrete publish topic is covered by
// a subscription the session still wants or must still clean up: an active
// broker subscription, a desired filter, or an exact pending managed-history filter.
// Both maps/plans are keyed by topic FILTERS (possibly wildcarded), so the
// concrete topic is matched against each filter with the same MQTT
// topic-filter logic the router uses for dispatch. Such a topic is never an
// orphan, only a route whose handler has not registered yet. Callers must
// hold s.mu.
//
// Before the FIRST Reconcile of a process lifetime (s.plan == nil), EVERY
// topic is treated as covered. Manager.Run calls Start before
// Reconcile, so a resumed clean_start=false broker replays the offline
// QoS 1/2 backlog on CONNACK while no plan is stashed yet; if that window
// outlives the grace timer (a reloadGate held by a concurrent operation, or
// an evicted SessionConnected event), a nil-plan coverage of "nothing" would
// classify the LIVE backlog as orphans — PUBACK-dropped and their topics
// UNSUBSCRIBED — an acknowledged live-route loss miscounted as benign
// cleanup. Covering everything retains the backlog un-acked (bounded by
// receive_maximum) until the first Reconcile stashes a plan; the plan is
// stashed even on the Reconcile error path, and reclassifyPending re-sweeps
// retained entries once real coverage is known, so genuine orphans are still
// converged — one reconcile later, never before the desired state exists.
// A session that never reconciles (sender-only construction without a
// manager) retains nothing in practice: it has no subscriptions, so a
// broker delivering to it is already a configuration error.
func (s *Session) topicCoveredLocked(topic string) bool {
	if s.plan == nil {
		return true
	}
	for filter := range s.activeSubs {
		if matchTopicFilter(filter, topic) {
			return true
		}
	}
	if s.plan != nil {
		for _, sub := range s.plan.Subscriptions {
			if matchTopicFilter(sub.Topic, topic) {
				return true
			}
		}
	}
	// Exact durable history remains coverage-protecting while a stale wildcard
	// or shared filter is pending cleanup. Otherwise a slow/failed UNSUBSCRIBE
	// can outlive startup grace and make the router ACK-drop traffic the broker
	// still delivers through that filter. Never infer filters from the concrete
	// topic; match only persisted exact filters.
	for filter := range s.managedHistory {
		if matchTopicFilter(filter, topic) {
			return true
		}
	}
	return false
}

// planHasSharedSubscriptionsLocked reports whether the last reconciled plan
// contains at least one shared subscription ("$share/<group>/<filter>"). It is
// the signal that this session participates in horizontal scale-out, which
// REQUIRES a unique per-instance client_id: it drives the one-time
// reconcile advisory and escalates the severity of a session takeover (a
// takeover while shared subscriptions are active is the observable symptom of
// replicas sharing a client_id and DOSing each other). Callers must hold s.mu.
func (s *Session) planHasSharedSubscriptionsLocked() bool {
	if s.plan == nil {
		return false
	}
	for _, sub := range s.plan.Subscriptions {
		if isSharedSubscriptionFilter(sub.Topic) {
			return true
		}
	}
	return false
}

// topicCovered is the router's covered-topic predicate (wired via
// withCovered in NewSession). It reports whether a concrete publish topic
// is still covered by a subscription the session wants — so the router can
// distinguish a REAL live-route loss (a covered topic acked-and-dropped
// past grace because its receiver handler registered late) from benign
// orphan cleanup, and split the drop metric accordingly. It must be
// called WITHOUT r.mu held (it takes s.mu); both router drop sites release
// r.mu before invoking it.
func (s *Session) topicCovered(topic string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.topicCoveredLocked(topic)
}

package paho

import (
	"context"
	"maps"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// orphanUnsubscribeTimeout bounds a single best-effort UNSUBSCRIBE issued
// for an orphan topic (a broker-retained subscription for a route removed
// from config). It is deliberately short: the ack-and-drop in the router
// already unblocked the in-flight window, so this converge step must not
// hang Close.
const orphanUnsubscribeTimeout = 10 * time.Second

// Reconcile diffs the desired SessionPlan against current subscriptions and
// issues Subscribe / Unsubscribe to reach the desired state.
//
// When the new plan has no subscriptions and a plan is already set (from a
// prior Reconcile call), the call is a no-op. This prevents a SessionManager
// with an empty plan from unsubscribing externally-managed topics.
func (s *Session) Reconcile(ctx context.Context, plan connectivity.SessionPlan) error {
	s.mu.Lock()
	hasPriorPlan := s.plan != nil
	if len(plan.Subscriptions) > 0 || !hasPriorPlan {
		s.plan = &plan
	}
	cm := s.cm
	s.mu.Unlock()

	if cm == nil {
		return shared.ErrUnavailable.WithMessage("session not started")
	}

	if len(plan.Subscriptions) == 0 && hasPriorPlan {
		return nil
	}

	if err := s.reconcile(ctx, cm, plan); err != nil {
		return err
	}

	// A reconcile actually ran and succeeded: the plan's subscriptions are
	// (re)established on the broker. Signal SessionReconciled from this
	// single owner. Per finding C7 the runtime session manager drives
	// Reconcile on every SessionConnected, so emitting here (rather than
	// inline in OnConnectionUp) is what preserves the "all subscriptions
	// re-established after reconnect" contract (ports.SessionReconciled)
	// on reconnect. The no-op early return above deliberately does NOT
	// emit: an empty plan that only re-affirms a prior plan re-established
	// nothing. A genuine reconcile that established zero NEW topics (the
	// delta was already satisfied) still signals reconciled.
	s.pushEvent(ports.SessionReconciled, nil)
	return nil
}

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
// name. A wildcard orphan subscription may therefore survive (UNSUBSCRIBE
// matches the filter, not a concrete topic), but its publishes keep being
// acked-and-dropped, so the stall cannot recur. The router dedups per
// topic, so this runs at most once per orphan topic per process.
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
	s.mu.Lock()
	cm := s.cm
	closed := s.closed
	covered := s.topicCoveredLocked(topic)
	s.mu.Unlock()
	if cm == nil || closed {
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

	ctx, cancel := context.WithTimeout(context.Background(), orphanUnsubscribeTimeout)
	defer cancel()

	if err := cm.Unsubscribe(ctx, []string{topic}); err != nil {
		if s.logger != nil {
			s.logger.Warn("mqtt: failed to unsubscribe orphan topic",
				"client_id", s.opts.ClientID,
				"topic", topic,
				"error", MapError(err),
			)
		}
		return
	}

	s.mu.Lock()
	delete(s.activeSubs, topic)
	s.mu.Unlock()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: unsubscribed orphan topic",
			"client_id", s.opts.ClientID,
			"topic", topic,
		)
	}
}

// topicCoveredLocked reports whether a concrete publish topic is covered by
// a subscription the session still wants — either an active broker
// subscription (s.activeSubs) or a desired filter in the current plan.
// Both maps/plans are keyed by topic FILTERS (possibly wildcarded), so the
// concrete topic is matched against each filter with the same MQTT
// topic-filter logic the router uses for dispatch. Such a topic is never an
// orphan, only a route whose handler has not registered yet. An empty
// activeSubs and a nil plan therefore cover nothing (every unmatched topic
// is a genuine orphan). Callers must hold s.mu.
func (s *Session) topicCoveredLocked(topic string) bool {
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
	return false
}

// topicCovered is the router's covered-topic predicate (wired via
// withCovered in NewSession). It reports whether a concrete publish topic
// is still covered by a subscription the session wants — so the router can
// distinguish a REAL live-route loss (a covered topic acked-and-dropped
// past grace because its receiver handler registered late) from benign
// orphan cleanup, and split the drop metric accordingly (M-3). It must be
// called WITHOUT r.mu held (it takes s.mu); both router drop sites release
// r.mu before invoking it.
func (s *Session) topicCovered(topic string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.topicCoveredLocked(topic)
}

func (s *Session) reconcile(ctx context.Context, cm pahoConnection, plan connectivity.SessionPlan) error {
	reconcileStart := s.clock().Now()
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	desired := make(map[string]byte, len(plan.Subscriptions))
	for _, sub := range plan.Subscriptions {
		desired[sub.Topic] = byte(sub.QoS)
	}

	s.mu.Lock()
	current := maps.Clone(s.activeSubs)
	s.mu.Unlock()
	if current == nil {
		current = make(map[string]byte)
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: reconcile",
			"client_id", s.opts.ClientID,
			"desired", len(desired),
			"active", len(current),
		)
	}

	// Unsubscribe topics no longer desired
	var toUnsub []string
	for topic := range current {
		if _, ok := desired[topic]; !ok {
			toUnsub = append(toUnsub, topic)
		}
	}

	if len(toUnsub) > 0 {
		if logging.TraceEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelTrace, "mqtt: unsubscribing",
				"client_id", s.opts.ClientID, "topics", toUnsub)
		}
		if err := cm.Unsubscribe(ctx, toUnsub); err != nil {
			return MapError(err)
		}
		s.mu.Lock()
		for _, t := range toUnsub {
			delete(s.activeSubs, t)
		}
		s.mu.Unlock()
	}

	// Subscribe to new or changed topics
	var toSub []subscribeSpec
	for topic, qos := range desired {
		curQoS, exists := current[topic]
		if !exists || curQoS != qos {
			toSub = append(toSub, subscribeSpec{Topic: topic, QoS: qos})
		}
	}

	if len(toSub) > 0 {
		if logging.TraceEnabled(s.logger) {
			topics := make([]string, len(toSub))
			for i, sub := range toSub {
				topics[i] = sub.Topic
			}
			s.logger.Log(ctx, logging.LevelTrace, "mqtt: subscribing",
				"client_id", s.opts.ClientID, "topics", topics)
		}
		reasons, err := cm.Subscribe(ctx, toSub)
		if err != nil {
			return MapError(err)
		}
		// Walk every reason code so we persist EVERY accepted topic
		// even when an earlier one was rejected. Without this, an
		// early-return on the first rejected reason would leave the
		// broker holding subscriptions our local activeSubs map does
		// not know about (BUG-RPS) — the next reconcile delta would
		// then re-subscribe and the delta accounting would be wrong.
		succeeded, firstErr, errTopic := classifySubackReasons(toSub, reasons)
		if len(succeeded) > 0 {
			s.mu.Lock()
			for _, opt := range succeeded {
				s.activeSubs[opt.Topic] = opt.QoS
			}
			s.mu.Unlock()
		}
		if firstErr != nil {
			return firstErr.With("topic", errTopic)
		}
	}

	elapsed := s.clock().Since(reconcileStart)
	s.metrics.Timer(MetricMQTTReconcileLatency, elapsed,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: reconcile done",
			"client_id", s.opts.ClientID,
			"unsubscribed", len(toUnsub),
			"subscribed", len(toSub),
			"duration", elapsed,
		)
	}

	return nil
}

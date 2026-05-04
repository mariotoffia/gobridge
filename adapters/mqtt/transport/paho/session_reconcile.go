package paho

import (
	"context"
	"maps"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
)

// Reconcile diffs the desired SessionPlan against current subscriptions and
// issues Subscribe / Unsubscribe to reach the desired state.
//
// When the new plan has no subscriptions and a plan is already set (from a
// prior Reconcile call), the call is a no-op. This prevents a SessionManager
// with an empty plan from unsubscribing externally-managed topics.
func (s *Session) Reconcile(ctx context.Context, plan domain.SessionPlan) error {
	s.mu.Lock()
	hasPriorPlan := s.plan != nil
	if len(plan.Subscriptions) > 0 || !hasPriorPlan {
		s.plan = &plan
	}
	cm := s.cm
	s.mu.Unlock()

	if cm == nil {
		return domain.ErrUnavailable.WithMessage("session not started")
	}

	if len(plan.Subscriptions) == 0 && hasPriorPlan {
		return nil
	}

	return s.reconcile(ctx, cm, plan)
}

func (s *Session) reconcile(ctx context.Context, cm *autopaho.ConnectionManager, plan domain.SessionPlan) error {
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
		if _, err := cm.Unsubscribe(ctx, &pahov5.Unsubscribe{Topics: toUnsub}); err != nil {
			return MapError(err)
		}
		s.mu.Lock()
		for _, t := range toUnsub {
			delete(s.activeSubs, t)
		}
		s.mu.Unlock()
	}

	// Subscribe to new or changed topics
	var toSub []pahov5.SubscribeOptions
	for topic, qos := range desired {
		curQoS, exists := current[topic]
		if !exists || curQoS != qos {
			toSub = append(toSub, pahov5.SubscribeOptions{Topic: topic, QoS: qos})
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
		sa, err := cm.Subscribe(ctx, &pahov5.Subscribe{Subscriptions: toSub})
		if err != nil {
			return MapError(err)
		}
		// Walk every reason code so we persist EVERY accepted topic
		// even when an earlier one was rejected. Without this, an
		// early-return on the first rejected reason would leave the
		// broker holding subscriptions our local activeSubs map does
		// not know about (BUG-RPS) — the next reconcile delta would
		// then re-subscribe and the delta accounting would be wrong.
		succeeded, firstErr, errTopic := classifySubackReasons(toSub, sa.Reasons)
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
	s.metrics.Timer(domain.MetricMQTTReconcileLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: s.opts.ClientID})
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

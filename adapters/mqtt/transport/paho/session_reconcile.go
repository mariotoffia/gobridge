package paho

import (
	"context"
	"maps"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

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

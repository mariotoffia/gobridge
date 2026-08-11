package paho

import (
	"context"
	"maps"
	"sort"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

func (s *Session) reconcile(
	ctx context.Context,
	cm pahoConnection,
	plan connectivity.SessionPlan,
	priorTopics []string,
	operationEpoch uint64,
) error {
	reconcileStart := s.clock().Now()

	desired := make(map[string]byte, len(plan.Subscriptions))
	for _, sub := range plan.Subscriptions {
		qos := byte(sub.QoS)
		if current, ok := desired[sub.Topic]; !ok || qos > current {
			desired[sub.Topic] = qos
		}
	}

	s.mu.Lock()
	if err := reconcileEpochMismatch(operationEpoch, s.connEpoch); err != nil {
		s.mu.Unlock()
		return err
	}
	current := maps.Clone(s.activeSubs)
	observed := maps.Clone(s.observedSubs)
	if observed == nil {
		observed = make(map[string]subscriptionGrant)
	}
	// Hand-built legacy sessions may only seed activeSubs. Treat those entries
	// as equal requested/granted observations for delta and cleanup purposes.
	for topic, qos := range current {
		if _, ok := observed[topic]; !ok {
			observed[topic] = subscriptionGrant{Requested: qos, Granted: qos}
		}
	}
	// The maps and cm belong to operationEpoch captured by Reconcile. Never
	// re-capture a newer generation here: pairing old cm with replacement state
	// would make an unchanged plan appear converged after Reload.
	s.mu.Unlock()
	if current == nil {
		current = make(map[string]byte)
	}

	s.mu.Lock()
	managed := s.managedRequired
	managedLoaded := s.managedLoaded
	managedStore := s.managedStore
	managedIdentity := s.managedIdentity
	managedHistory := maps.Clone(s.managedHistory)
	cleanupVerification := maps.Clone(s.managedCleanupVerification)
	s.mu.Unlock()
	if managed {
		if !managedLoaded || managedStore == nil || managedIdentity == "" {
			return shared.ErrUnavailable.WithMessage("mqtt: managed subscription history was not loaded before reconcile")
		}
		if len(cleanupVerification) > 0 {
			verified := make([]string, 0, len(cleanupVerification))
			for filter := range cleanupVerification {
				verified = append(verified, filter)
			}
			sort.Strings(verified)
			if err := s.verifyManagedReplay(ctx, verified); err != nil {
				return err
			}
			if err := s.finalizeManagedCleanup(ctx, managedStore, managedIdentity, verified); err != nil {
				return err
			}
			for _, filter := range verified {
				delete(managedHistory, filter)
			}
		}
		candidates := make([]string, 0, len(desired))
		for filter := range desired {
			candidates = append(candidates, filter)
		}
		sort.Strings(candidates)
		if len(candidates) > 0 {
			// Write-ahead is load-bearing: a crash after SUBSCRIBE but before
			// local state commit must still leave the exact filter removable.
			if err := managedStore.Remember(ctx, managedIdentity, candidates); err != nil {
				return shared.ErrUnavailable.WithMessage("mqtt: remember managed subscription candidates before SUBSCRIBE").Wrap(err)
			}
			if managedHistory == nil {
				managedHistory = make(map[string]struct{})
			}
			for _, filter := range candidates {
				managedHistory[filter] = struct{}{}
			}
			s.mu.Lock()
			for _, filter := range candidates {
				s.managedHistory[filter] = struct{}{}
			}
			s.mu.Unlock()
		}
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: reconcile",
			"client_id", s.opts.ClientID,
			"desired", len(desired),
			"active", len(current),
			"observed", len(observed),
		)
	}

	// Unsubscribe topics no longer desired. Candidates are the UNION of the
	// broker-observed successful grants, contract-active subscriptions, AND the
	// prior plan's desired topics (priorTopics). The prior-plan topics matter in
	// the reconnect window: handleConnectionUp reset activeSubs to empty but a
	// clean_start=false broker still holds the resumed subscriptions, so
	// without them an empty plan would fail to tear anything down and orphan
	// the broker sub (c4-remove-subs). Unsubscribing a topic the broker does
	// not actually hold is harmless (the broker ignores it).
	var toUnsub []string
	unsubSeen := make(map[string]struct{})
	if managed {
		// Durable cleanup is derived ONLY from exact persisted filters. Never
		// infer a wildcard/shared filter from a delivered concrete topic.
		for filter := range managedHistory {
			if _, wanted := desired[filter]; !wanted {
				toUnsub = append(toUnsub, filter)
			}
		}
	} else {
		for topic := range observed {
			if _, ok := desired[topic]; !ok {
				toUnsub = append(toUnsub, topic)
				unsubSeen[topic] = struct{}{}
			}
		}
		for topic := range current {
			if _, wanted := desired[topic]; wanted {
				continue
			}
			if _, seen := unsubSeen[topic]; seen {
				continue
			}
			toUnsub = append(toUnsub, topic)
			unsubSeen[topic] = struct{}{}
		}
		for _, topic := range priorTopics {
			if _, ok := desired[topic]; ok {
				continue
			}
			if _, dup := unsubSeen[topic]; dup {
				continue
			}
			toUnsub = append(toUnsub, topic)
			unsubSeen[topic] = struct{}{}
		}
	}
	sort.Strings(toUnsub)

	if len(toUnsub) > 0 && !managed {
		confirmation, err := s.unsubscribeConfirmed(ctx, cm, toUnsub, operationEpoch)
		if err != nil {
			return err
		}
		if err := s.removeObservedSubscriptions(operationEpoch, confirmation.confirmed); err != nil {
			return err
		}
		if confirmation.firstErr != nil {
			return confirmation.firstErr.With("topic", confirmation.errTopic)
		}
	}

	// Subscribe to new or changed topics
	var toSub []subscribeSpec
	for topic, qos := range desired {
		grant, exists := observed[topic]
		if !exists || grant.Requested != qos {
			// No-Local is opt-in per session (no_local config, default off).
			// When enabled it breaks the same-session MQTT->MQTT self-delivery
			// loop (Scenario 01) but MUST stay off for a shared subscription
			// ($share), where it is an MQTT 5 Protocol Error the broker rejects
			// with a DISCONNECT.
			//
			// RetainHandling is 1 for persistent/exclusive sessions so a
			// reconnect that resumes the session does not trigger a full
			// retained-message replay per filter; ephemeral sessions keep
			// 0 (each connect is a fresh subscription that must rehydrate
			// retained state).
			toSub = append(toSub, subscribeSpec{
				Topic:          topic,
				QoS:            qos,
				NoLocal:        s.opts.NoLocal && !isSharedSubscriptionFilter(topic),
				RetainHandling: retainHandlingForMode(s.mode),
			})
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
		// Adapter-owned deadline per SUBSCRIBE too: a broker that accepts the
		// connection but never returns SUBACK must not hang the reconcile — and
		// any startup / hot-reload step awaiting it — indefinitely.
		if err := s.requireReconcileEpoch(operationEpoch); err != nil {
			return err
		}
		subCtx, cancel := context.WithTimeout(ctx, s.reconcileTimeout())
		reasons, err := cm.Subscribe(subCtx, toSub)
		cancel()
		if err != nil {
			return MapError(err)
		}
		if err := s.requireReconcileEpoch(operationEpoch); err != nil {
			return err
		}
		// Walk every reason code so every successful broker grant is observed,
		// while only contract-satisfying grants become active. This preserves
		// cleanup knowledge without treating a weaker QoS grant as ready.
		succeeded, firstErr, errTopic := classifySubackReasons(toSub, reasons)
		var downgradeErr *shared.BridgeError
		for _, opt := range succeeded {
			req := desired[opt.Topic]
			if opt.QoS >= req {
				continue
			}

			s.metrics.Counter(MetricMQTTQoSDowngraded, 1,
				shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
			if s.logger != nil {
				s.logger.Warn("mqtt: broker downgraded subscription QoS below requested; "+
					"delivery guarantee is weaker than the route assumes",
					"client_id", s.opts.ClientID,
					"topic", opt.Topic,
					"requested_qos", req,
					"granted_qos", opt.QoS,
				)
			}
			if downgradeErr == nil {
				downgradeErr = qosDowngradeError(opt.Topic, req, opt.QoS)
			}
		}
		if len(succeeded) > 0 {
			s.mu.Lock()
			if epochErr := reconcileEpochMismatch(operationEpoch, s.connEpoch); epochErr != nil {
				s.mu.Unlock()
				return epochErr
			}
			if s.observedSubs == nil {
				s.observedSubs = make(map[string]subscriptionGrant)
			}
			for _, opt := range succeeded {
				req := desired[opt.Topic]
				s.observedSubs[opt.Topic] = subscriptionGrant{Requested: req, Granted: opt.QoS}
				if opt.QoS >= req {
					s.activeSubs[opt.Topic] = opt.QoS
				} else {
					delete(s.activeSubs, opt.Topic)
				}
			}
			s.mu.Unlock()
		}
		if downgradeErr != nil {
			return downgradeErr
		}
		if firstErr != nil {
			return firstErr.With("topic", errTopic)
		}
	}

	if managed && len(toUnsub) > 0 {
		if err := s.reconcileManagedUnsubscribe(ctx, cm, toUnsub, operationEpoch, managedStore, managedIdentity); err != nil {
			return err
		}
	}

	if err := s.requireReconcileEpoch(operationEpoch); err != nil {
		return err
	}
	if downgradeErr := observedQoSDowngrade(desired, observed); downgradeErr != nil {
		return downgradeErr
	}

	s.mu.Lock()
	if epochErr := reconcileEpochMismatch(operationEpoch, s.connEpoch); epochErr != nil {
		s.mu.Unlock()
		return epochErr
	}
	s.subscriptionsSatisfied = subscriptionStateConverged(desired, s.observedSubs, s.activeSubs)
	s.mu.Unlock()

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

func subscriptionStateConverged(
	desired map[string]byte,
	observed map[string]subscriptionGrant,
	active map[string]byte,
) bool {
	if len(observed) != len(desired) || len(active) != len(desired) {
		return false
	}
	for topic, requested := range desired {
		grant, ok := observed[topic]
		if !ok || grant.Requested != requested || grant.Granted < requested {
			return false
		}
		qos, ok := active[topic]
		if !ok || qos != grant.Granted {
			return false
		}
	}
	return true
}

func observedQoSDowngrade(
	desired map[string]byte,
	observed map[string]subscriptionGrant,
) *shared.BridgeError {
	topics := make([]string, 0, len(desired))
	for topic := range desired {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	for _, topic := range topics {
		req := desired[topic]
		grant, ok := observed[topic]
		if ok && grant.Requested == req && grant.Granted < req {
			return qosDowngradeError(topic, req, grant.Granted)
		}
	}
	return nil
}

func (s *Session) requireReconcileEpoch(operationEpoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return reconcileEpochMismatch(operationEpoch, s.connEpoch)
}

func reconcileEpochMismatch(operationEpoch, currentEpoch uint64) error {
	if operationEpoch == currentEpoch {
		return nil
	}
	return shared.ErrUnavailable.
		WithMessage("mqtt: connection changed during reconcile").
		With("operation_epoch", operationEpoch).
		With("current_epoch", currentEpoch)
}

func qosDowngradeError(topic string, requested, granted byte) *shared.BridgeError {
	return shared.ErrQoSNotSupported.
		WithMessage("mqtt: broker granted subscription QoS below requested").
		With("topic", topic).
		With("requested_qos", int(requested)).
		With("granted_qos", int(granted))
}

// retainHandlingForMode returns the MQTT5 RetainHandling value for a session of
// the given mode. Persistent and exclusive sessions resume across
// reconnects, so RetainHandling 1 ("send retained only if the subscription did
// not already exist") hydrates retained state on the first subscribe yet
// suppresses a redundant retained replay on every subsequent reconnect that
// resumes the session. Ephemeral sessions start clean each connect and use 0
// (always send retained), because their subscription never pre-exists.
func retainHandlingForMode(mode connectivity.SessionMode) byte {
	if mode == connectivity.SessionEphemeral {
		return 0
	}
	return 1
}

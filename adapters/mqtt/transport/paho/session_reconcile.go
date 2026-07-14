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
// An EMPTY target plan is treated as an intentional "remove all
// subscriptions" (e.g. hot reconfig removed the last MQTT receiver): the
// subscriptions the prior plan desired are UNSUBSCRIBED. The teardown is
// gated on whether the PRIOR PLAN held subscriptions (desired-state history),
// not on the volatile activeSubs snapshot a reconnect may have just reset —
// so a subscription resumed by a clean_start=false broker is torn down even
// in the post-reconnect window (c4-remove-subs). Only a subless transition —
// an empty plan re-affirming a prior plan that had no subscriptions (a
// sender-only session) — is a no-op, so a SessionManager that never had
// subscriptions cannot churn the broker.
func (s *Session) Reconcile(ctx context.Context, plan connectivity.SessionPlan) error {
	s.mu.Lock()
	// The empty-plan no-op and the reconnect-window teardown key off the
	// last SUCCESSFULLY APPLIED plan (appliedPlan), NOT the desired plan
	// (s.plan) that is overwritten below (blocking-#2). Committing the
	// desired plan as history before the broker ops succeed is exactly the
	// bug: a failed Unsubscribe would leave s.plan empty, and the NEXT empty
	// Reconcile would no-op instead of RETRYING the unsubscribe.
	appliedExists := s.appliedPlan != nil
	appliedHadSubs := appliedExists && len(s.appliedPlan.Subscriptions) > 0
	// Snapshot the last-APPLIED desired topics: an empty target plan must tear
	// these down even when a reconnect just reset the volatile activeSubs
	// snapshot to empty (a clean_start=false broker resumed them). Using the
	// APPLIED plan (not the desired one) means a reconcile that FAILED to
	// unsubscribe leaves these topics in the retry set for the next reconcile.
	priorPlanTopics := s.appliedPlanTopicsLocked()
	// Record the latest desired plan unconditionally — including an EMPTY plan.
	// This is the desired-state stash OnConnectionUp replays on (re)connect and
	// the source topicCovered consults; it is set even on the error path so a
	// Reconcile-before-Start still stashes the plan. It is deliberately NOT the
	// applied history (see appliedPlan, set only after the broker ops succeed).
	s.plan = &plan
	// Shared-subscription scale-out on a stable/shared-ClientID mode is the
	// client_id-collision footgun (HIGH-3): every replica MUST use a UNIQUE
	// client_id, else they form a single broker session and take each other
	// over instead of load-balancing. We cannot see the other replicas'
	// ClientIDs from one process, so surface the requirement once. Ephemeral
	// sessions already get a unique ClientID + CleanStart, so they are the
	// correctly-configured scale-out shape and are not warned.
	warnSharedSubs := s.planHasSharedSubscriptionsLocked() &&
		s.mode != connectivity.SessionEphemeral && !s.sharedSubWarned
	if warnSharedSubs {
		s.sharedSubWarned = true
	}
	cm := s.cm
	s.mu.Unlock()

	if warnSharedSubs && s.logger != nil {
		s.logger.Warn("mqtt: shared subscriptions ($share) configured on a stable-client_id session — "+
			"horizontal scale-out REQUIRES a UNIQUE client_id per instance; replicas that reuse this client_id "+
			"form one broker session and take each other over (self-DOS) instead of load-balancing. A shared "+
			"client_id is only safe behind an exclusive lease (a single active owner), which serialises rather "+
			"than scales the subscription",
			"client_id", s.opts.ClientID,
			"session_mode", s.mode,
		)
	}

	if cm == nil {
		return shared.ErrUnavailable.WithMessage("session not started")
	}

	// An empty target plan is an intentional "remove all subscriptions" (e.g.
	// hot reconfig removed the last MQTT receiver): the managed subscriptions
	// this session established MUST be UNSUBSCRIBED, else the broker keeps
	// delivering on stale subscriptions the router then ack-drops as orphans
	// forever (c4-remove-subs).
	//
	// The teardown is gated on the last-APPLIED history (whether the plan we
	// last SUCCESSFULLY reconciled held subscriptions), NOT on the volatile
	// activeSubs snapshot and NOT on the desired plan. handleConnectionUp
	// resets activeSubs to empty on every reconnect while a clean_start=false
	// broker still holds the resumed subscriptions, so an empty plan reconciled
	// in that post-reset/pre-resubscribe window would look like "nothing to
	// remove" under an activeSubs guard and orphan the broker sub until the
	// router's grace-sweep backstop. Gating on the applied plan closes that
	// window: s.reconcile unsubscribes the applied desired topics
	// (priorPlanTopics) even when activeSubs is empty.
	//
	// Only a genuinely subless transition — an empty plan re-affirming an
	// APPLIED plan that itself held no subscriptions (a sender-only session) —
	// is a true no-op, so a SessionManager that never had subscriptions cannot
	// churn the broker. Because the no-op keys off APPLIED (not desired) state,
	// a FAILED unsubscribe (whose applied plan still holds subscriptions) is
	// NOT mistaken for a settled subless session and IS retried (blocking-#2).
	if len(plan.Subscriptions) == 0 && appliedExists && !appliedHadSubs {
		return nil
	}

	if err := s.reconcile(ctx, cm, plan, priorPlanTopics); err != nil {
		return err
	}

	// The broker ops SUCCEEDED: commit this plan as the last-applied history so
	// the next empty-plan reconcile can no-op and the reconnect-window teardown
	// tears down the right topics. A FAILED reconcile returned above WITHOUT
	// reaching here, so appliedPlan stays at the last successful value and the
	// next reconcile retries the failed op (blocking-#2).
	applied := plan
	s.mu.Lock()
	s.appliedPlan = &applied
	s.mu.Unlock()

	// A successful reconcile may have REMOVED coverage (an Unsubscribe tore
	// down a topic a receiver was removed from config). A publish RETAINED as
	// covered past the grace window on that topic is now a TRUE ORPHAN, but
	// nothing else re-sweeps it — it would stay un-acked forever, pinning the
	// broker Receive-Maximum window and wedging ingress (blocking-#1). Re-run
	// the router's settle pass so any now-uncovered pending entry is
	// reclassified (acked, dropped, unsubscribed) while still-covered entries
	// stay put. This runs with NO session mutex held (reclassifyPending takes
	// r.mu then releases it before calling covered(), preserving lock order),
	// and is a cheap no-op when the pending buffer is empty (the steady state).
	if s.router != nil {
		s.router.reclassifyPending()
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

// appliedPlanTopicsLocked returns the topic filters of the last SUCCESSFULLY
// reconciled plan (s.appliedPlan), or nil when no plan has been applied yet. It
// is the applied-state history Reconcile hands to s.reconcile so an empty
// target plan tears down the topics actually established on the broker — even
// when a reconnect has just reset the volatile activeSubs snapshot
// (c4-remove-subs) AND even when a prior reconcile FAILED to unsubscribe them
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

// planHasSharedSubscriptionsLocked reports whether the last reconciled plan
// contains at least one shared subscription ("$share/<group>/<filter>"). It is
// the signal that this session participates in horizontal scale-out, which
// REQUIRES a unique per-instance client_id (HIGH-3): it drives the one-time
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
// orphan cleanup, and split the drop metric accordingly (M-3). It must be
// called WITHOUT r.mu held (it takes s.mu); both router drop sites release
// r.mu before invoking it.
func (s *Session) topicCovered(topic string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.topicCoveredLocked(topic)
}

// reconcileTimeout returns the adapter-owned deadline applied to EACH broker
// SUBSCRIBE / UNSUBSCRIBE during reconciliation (HIGH-2). A non-positive
// configured value is coerced to DefaultReconcileTimeout: this is a liveness
// safety bound (a wedged broker whose SUBACK/UNSUBACK never arrives must not
// hang the reconcile, nor the startup / hot-reload step awaiting it), so unlike
// the tuning knobs it cannot be disabled with an explicit 0.
func (s *Session) reconcileTimeout() time.Duration {
	if s.opts.ReconcileTimeout > 0 {
		return s.opts.ReconcileTimeout
	}
	return DefaultReconcileTimeout
}

func (s *Session) reconcile(ctx context.Context, cm pahoConnection, plan connectivity.SessionPlan, priorTopics []string) error {
	reconcileStart := s.clock().Now()
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	desired := make(map[string]byte, len(plan.Subscriptions))
	for _, sub := range plan.Subscriptions {
		qos := byte(sub.QoS)
		if current, ok := desired[sub.Topic]; !ok || qos > current {
			desired[sub.Topic] = qos
		}
	}

	s.mu.Lock()
	current := maps.Clone(s.activeSubs)
	// Capture the connection generation alongside the snapshot. Every
	// activeSubs write-back below is gated on this still matching: a reconnect
	// (handleConnectionUp) landing mid-reconcile bumps s.connEpoch and resets
	// activeSubs, so a stale write-back must be abandoned rather than pollute
	// the fresh set (A-3).
	startEpoch := s.connEpoch
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

	// Unsubscribe topics no longer desired. Candidates are the UNION of the
	// active broker subscriptions we know about (current) AND the prior plan's
	// desired topics (priorTopics). The prior-plan topics matter in the
	// reconnect window: handleConnectionUp reset activeSubs to empty but a
	// clean_start=false broker still holds the resumed subscriptions, so
	// without them an empty plan would fail to tear anything down and orphan
	// the broker sub (c4-remove-subs). Unsubscribing a topic the broker does
	// not actually hold is harmless (the broker ignores it).
	var toUnsub []string
	unsubSeen := make(map[string]struct{})
	for topic := range current {
		if _, ok := desired[topic]; !ok {
			toUnsub = append(toUnsub, topic)
			unsubSeen[topic] = struct{}{}
		}
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

	if len(toUnsub) > 0 {
		if logging.TraceEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelTrace, "mqtt: unsubscribing",
				"client_id", s.opts.ClientID, "topics", toUnsub)
		}
		// Wrap the broker op in an adapter-owned deadline: the reconcile ctx may
		// carry none, so a wedged broker (UNSUBACK never arrives on a half-open
		// connection) would otherwise hang the reconcile indefinitely (HIGH-2).
		unsubCtx, cancel := context.WithTimeout(ctx, s.reconcileTimeout())
		err := cm.Unsubscribe(unsubCtx, toUnsub)
		cancel()
		if err != nil {
			return MapError(err)
		}
		s.mu.Lock()
		if s.connEpoch == startEpoch {
			for _, t := range toUnsub {
				delete(s.activeSubs, t)
			}
		}
		s.mu.Unlock()
	}

	// Subscribe to new or changed topics
	var toSub []subscribeSpec
	for topic, qos := range desired {
		curQoS, exists := current[topic]
		if !exists || curQoS != qos {
			// No-Local is opt-in per session (no_local config, default off).
			// When enabled it breaks the same-session MQTT->MQTT self-delivery
			// loop (Scenario 01) but MUST stay off for a shared subscription
			// ($share), where it is an MQTT 5 Protocol Error the broker rejects
			// with a DISCONNECT (A-2).
			//
			// RetainHandling is 1 for persistent/exclusive sessions so a
			// reconnect that resumes the session does not trigger a full
			// retained-message replay per filter (A-7); ephemeral sessions keep
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
		// any startup / hot-reload step awaiting it — indefinitely (HIGH-2).
		subCtx, cancel := context.WithTimeout(ctx, s.reconcileTimeout())
		reasons, err := cm.Subscribe(subCtx, toSub)
		cancel()
		if err != nil {
			return MapError(err)
		}
		// Walk every reason code so every contract-satisfying topic is persisted
		// even when a sibling is rejected or downgraded. This preserves partial
		// SUBACK success without treating a weaker QoS grant as active.
		succeeded, firstErr, errTopic := classifySubackReasons(toSub, reasons)
		accepted := make([]subscribeSpec, 0, len(succeeded))
		var downgradeErr *shared.BridgeError
		for _, opt := range succeeded {
			req := desired[opt.Topic]
			if opt.QoS >= req {
				accepted = append(accepted, opt)
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
				downgradeErr = shared.ErrQoSNotSupported.
					WithMessage("mqtt: broker granted subscription QoS below requested").
					With("topic", opt.Topic).
					With("requested_qos", int(req)).
					With("granted_qos", int(opt.QoS))
			}
		}
		if len(accepted) > 0 {
			s.mu.Lock()
			if s.connEpoch == startEpoch {
				for _, opt := range accepted {
					s.activeSubs[opt.Topic] = opt.QoS
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

// retainHandlingForMode returns the MQTT5 RetainHandling value for a session of
// the given mode (A-7). Persistent and exclusive sessions resume across
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

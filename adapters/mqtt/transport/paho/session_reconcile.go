package paho

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
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
func (s *Session) Reconcile(ctx context.Context, plan connectivity.SessionPlan) (retErr error) {
	ctx, cancelRecoveryBound, recoveryGeneration := s.recoveryReconcileContext(ctx)
	defer cancelRecoveryBound()
	if recoveryGeneration != 0 {
		if err := s.acquireReload(ctx); err != nil {
			return MapError(err).WithMessage("mqtt: recovery reconcile waiting for reload serialization")
		}
		defer s.releaseReload()
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	defer func() {
		if recoveryGeneration == 0 {
			return
		}
		if retErr != nil {
			s.completeRecoveryAttempt(recoveryGeneration, retErr, false)
			return
		}
		s.mu.Lock()
		resumed := s.recoveryAttemptActive &&
			s.recoveryGeneration == recoveryGeneration && s.recoverySessionPresent
		recoveryErr := s.recoveryErr
		s.mu.Unlock()
		if !resumed {
			if recoveryErr != nil {
				retErr = recoveryErr
			} else {
				retErr = shared.ErrUnavailable.WithMessage(
					"mqtt: settlement recovery reconciliation completed without resumed broker state")
			}
			s.completeRecoveryAttempt(recoveryGeneration, retErr, false)
			return
		}
		if !s.completeRecoveryAttempt(recoveryGeneration, nil, true) {
			retErr = shared.ErrUnavailable.WithMessage(
				"mqtt: settlement recovery completion raced its hard deadline")
		}
	}()
	// The exclusive manager releases its lease whenever Reconcile returns an
	// error. Detach and disconnect first so no broker consumer can survive past
	// that fencing boundary, including a replacement generation created by the
	// managed-history cleanup recycle below.
	defer func() {
		if retErr != nil && s.mode == connectivity.SessionExclusive {
			if disconnectErr := s.disconnectFailedReconcile(ctx); disconnectErr != nil &&
				!errors.Is(retErr, shared.ErrTransportClosedPermanently) {
				retErr = errors.Join(retErr, disconnectErr)
			}
		}
	}()

	desiredPlan := cloneSessionPlan(plan)
	s.mu.Lock()
	// The empty-plan no-op and the reconnect-window teardown key off the
	// last SUCCESSFULLY APPLIED plan (appliedPlan), NOT the desired plan
	// (s.plan) that is overwritten below (blocking-#2). Committing the
	// desired plan as history before the broker ops succeed is exactly the
	// bug: a failed Unsubscribe would leave s.plan empty, and the NEXT empty
	// Reconcile would no-op instead of RETRYING the unsubscribe.
	appliedExists := s.appliedPlan != nil
	appliedHadSubs := appliedExists && len(s.appliedPlan.Subscriptions) > 0
	observedHadSubs := len(s.observedSubs) > 0 || len(s.activeSubs) > 0
	managedHistoryRequired := s.managedRequired
	startEpoch := s.connEpoch
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
	s.plan = &desiredPlan
	// An explicit plan is unsatisfied until this operation proves exact broker
	// convergence. Errors and reconnect generation changes leave it false.
	s.subscriptionsSatisfied = false
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
	reconcileSnapshotHook := s.reconcileSnapshotHook
	s.mu.Unlock()

	// Install history-minus-desired in the router before any broker cleanup or
	// handler matching. History was initially gated in full before broker dial.
	s.syncManagedCleanupGate(desiredPlan)

	if reconcileSnapshotHook != nil {
		reconcileSnapshotHook()
	}

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
	// APPLIED plan that itself held no subscriptions (a sender-only session) and
	// has no broker-observed grants — is a true no-op, so a SessionManager that
	// never had subscriptions cannot churn the broker. Because the no-op keys off
	// APPLIED (not desired) state, a FAILED unsubscribe (whose applied plan
	// still holds subscriptions) is NOT mistaken for a settled subless session
	// and IS retried (blocking-#2). A managed session never takes this process-
	// local shortcut: durable history remains authoritative even after an empty
	// plan was applied, because write-ahead candidates may still need cleanup.
	if len(desiredPlan.Subscriptions) == 0 &&
		appliedExists && !appliedHadSubs && !observedHadSubs && !managedHistoryRequired {
		s.mu.Lock()
		if err := reconcileEpochMismatch(startEpoch, s.connEpoch); err != nil {
			s.mu.Unlock()
			return err
		}
		s.subscriptionsSatisfied = true
		s.mu.Unlock()
		return nil
	}

	for {
		err := s.reconcile(ctx, cm, desiredPlan, priorPlanTopics, startEpoch)
		if !errors.Is(err, errManagedCleanupRecycled) {
			if err != nil {
				return err
			}
			break
		}

		// Managed cleanup removed broker state and Reload established a new
		// connection. Reconcile that replacement generation while still holding
		// reconcileMu (and, for exclusive sessions, while the manager still owns
		// its lease). Calling public Reconcile recursively would deadlock.
		s.mu.Lock()
		cm = s.cm
		startEpoch = s.connEpoch
		s.mu.Unlock()
		if cm == nil {
			return shared.ErrUnavailable.WithMessage("mqtt: managed cleanup replacement connection is unavailable")
		}
	}

	// The broker ops SUCCEEDED: commit this plan as the last-applied history so
	// the next empty-plan reconcile can no-op and the reconnect-window teardown
	// tears down the right topics. A FAILED reconcile returned above WITHOUT
	// reaching here, so appliedPlan stays at the last successful value and the
	// next reconcile retries the failed op (blocking-#2).
	applied := cloneSessionPlan(desiredPlan)
	s.mu.Lock()
	if err := reconcileEpochMismatch(startEpoch, s.connEpoch); err != nil {
		s.mu.Unlock()
		return err
	}
	s.appliedPlan = &applied
	s.mu.Unlock()

	// Durable history may have changed after exact cleanup; narrow the router
	// gate before pending reclassification, but keep global recycle quiescence
	// until the replacement epoch is proved below.
	s.syncManagedCleanupGate(desiredPlan)

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

	if err := s.requireReconcileEpoch(startEpoch); err != nil {
		return err
	}
	if s.router != nil {
		if err := s.router.resumeManagedDispatch(ctx); err != nil {
			return err
		}
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

func cloneSessionPlan(plan connectivity.SessionPlan) connectivity.SessionPlan {
	plan.Subscriptions = slices.Clone(plan.Subscriptions)
	plan.Publishers = slices.Clone(plan.Publishers)
	plan.ExpectedReceiverIDs = slices.Clone(plan.ExpectedReceiverIDs)
	return plan
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
	// Use the reconciliation serialization boundary so an orphan decision and
	// destructive broker operation cannot interleave with a route re-add.
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	// Recheck coverage only after owning reconcileMu, immediately before the
	// broker operation. Reconcile holds the same lock while declaring its plan.
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

	ctx, cancel := context.WithTimeout(context.Background(), orphanUnsubscribeTimeout)
	defer cancel()

	reasons, err := cm.Unsubscribe(ctx, []string{topic})
	if err == nil {
		confirmation := classifyUnsubackReasons([]string{topic}, reasons)
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
// a subscription the session still wants or must still clean up: an active
// broker subscription, a desired filter, or an exact pending managed-history filter.
// Both maps/plans are keyed by topic FILTERS (possibly wildcarded), so the
// concrete topic is matched against each filter with the same MQTT
// topic-filter logic the router uses for dispatch. Such a topic is never an
// orphan, only a route whose handler has not registered yet. An empty
// activeSubs, a nil plan, and empty managed history therefore cover nothing
// (every unmatched topic is a genuine orphan). Callers must hold s.mu.
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

var errManagedCleanupRecycled = errors.New("mqtt: managed subscription cleanup recycled connection")

func (s *Session) reconcileManagedUnsubscribe(
	ctx context.Context,
	cm pahoConnection,
	toUnsub []string,
	operationEpoch uint64,
	managedStore ports.ManagedSubscriptionStore,
	managedIdentity string,
) error {
	confirmation, err := s.unsubscribeConfirmed(ctx, cm, toUnsub, operationEpoch)
	if err != nil {
		return err
	}
	if err := s.removeObservedSubscriptions(operationEpoch, confirmation.confirmed); err != nil {
		return err
	}

	if confirmation.removedAny {
		s.mu.Lock()
		if s.managedCleanupVerification == nil {
			s.managedCleanupVerification = make(map[string]struct{})
		}
		for _, filter := range confirmation.confirmed {
			s.managedCleanupVerification[filter] = struct{}{}
		}
		s.mu.Unlock()
		// Stop new handler dispatch and await both accepted callbacks and runtime
		// source settlement before disconnecting the old broker generation.
		if err := s.quiesceForRecycle(ctx); err != nil {
			return s.failClosedAfterQuiescence(ctx, err)
		}
		// Do NOT Forget durable history yet. MQTT brokers may pin an unacknowledged
		// QoS1 shared delivery to this persistent client session and replay it on
		// the replacement connection instead of redistributing it. The replacement
		// generation must prove no such delivery was buffered before history can be
		// removed; otherwise restoring the old config could not identify/drain it.
		if reloadErr := s.Reload(ctx); reloadErr != nil {
			return shared.ErrUnavailable.WithMessage("mqtt: recycle after managed subscription cleanup").Wrap(reloadErr)
		}
		if confirmation.firstErr != nil {
			if err := s.verifyManagedReplay(ctx, confirmation.confirmed); err != nil {
				return err
			}
			if err := s.finalizeManagedCleanup(ctx, managedStore, managedIdentity, confirmation.confirmed); err != nil {
				return err
			}
			return confirmation.firstErr.With("topic", confirmation.errTopic)
		}
		return errManagedCleanupRecycled
	}

	// UNSUBACK 0x11 proves only that the filter is absent NOW. After a crash, the
	// prior process may have received 0x00 and removed the filter but died before
	// recording in-memory verification/Forget; this replacement can still receive
	// a delayed broker-pinned replay. Always wait the current generation's full
	// replay-grace before Forget, even without managedCleanupVerification state.
	if err := s.verifyManagedReplay(ctx, confirmation.confirmed); err != nil {
		return err
	}
	if err := s.finalizeManagedCleanup(ctx, managedStore, managedIdentity, confirmation.confirmed); err != nil {
		return err
	}
	if confirmation.firstErr != nil {
		return confirmation.firstErr.With("topic", confirmation.errTopic)
	}
	return nil
}

func (s *Session) verifyManagedReplay(ctx context.Context, filters []string) error {
	if len(filters) == 0 || s.router == nil {
		return nil
	}
	pinned, err := s.router.awaitManagedReplay(ctx, filters)
	if err != nil || pinned {
		return s.failClosedForManagedMigration(ctx)
	}
	return nil
}

func (s *Session) finalizeManagedCleanup(
	ctx context.Context,
	managedStore ports.ManagedSubscriptionStore,
	managedIdentity string,
	filters []string,
) error {
	if len(filters) == 0 {
		return nil
	}
	if s.router != nil && s.router.pendingMatching(filters) > 0 {
		return s.failClosedForManagedMigration(ctx)
	}
	if err := managedStore.Forget(ctx, managedIdentity, filters); err != nil {
		return shared.ErrUnavailable.WithMessage("mqtt: forget verified managed subscriptions").Wrap(err)
	}
	s.mu.Lock()
	for _, filter := range filters {
		delete(s.managedHistory, filter)
		delete(s.managedCleanupVerification, filter)
	}
	s.mu.Unlock()
	return nil
}

type unsubscribeConfirmation struct {
	confirmed  []string
	removedAny bool
	firstErr   *shared.BridgeError
	errTopic   string
}

func (s *Session) unsubscribeConfirmed(ctx context.Context, cm pahoConnection, topics []string, operationEpoch uint64) (unsubscribeConfirmation, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "mqtt: unsubscribing", "client_id", s.opts.ClientID, "topics", topics)
	}
	if err := s.requireReconcileEpoch(operationEpoch); err != nil {
		return unsubscribeConfirmation{}, err
	}
	unsubCtx, cancel := context.WithTimeout(ctx, s.reconcileTimeout())
	reasons, err := cm.Unsubscribe(unsubCtx, topics)
	cancel()
	if err != nil {
		return unsubscribeConfirmation{}, MapError(err)
	}
	if err := s.requireReconcileEpoch(operationEpoch); err != nil {
		return unsubscribeConfirmation{}, err
	}
	return classifyUnsubackReasons(topics, reasons), nil
}

func classifyUnsubackReasons(topics []string, reasons []byte) unsubscribeConfirmation {
	result := unsubscribeConfirmation{confirmed: make([]string, 0, len(topics))}
	for i, topic := range topics {
		if i >= len(reasons) {
			if result.firstErr == nil {
				result.firstErr = shared.ErrProtocolError.WithMessage("mqtt: UNSUBACK returned fewer reason codes than requested filters")
				result.errTopic = topic
			}
			continue
		}
		switch reasons[i] {
		case 0x00:
			result.confirmed = append(result.confirmed, topic)
			result.removedAny = true
			continue
		case 0x11:
			result.confirmed = append(result.confirmed, topic)
			continue
		}
		berr := MapSubscribeReasonCode(reasons[i])
		if berr == nil {
			berr = shared.ErrProtocolError.WithMessage("mqtt: invalid UNSUBACK success reason code")
		}
		if result.firstErr == nil {
			result.firstErr = berr
			result.errTopic = topic
		}
	}
	return result
}

func (s *Session) removeObservedSubscriptions(operationEpoch uint64, topics []string) error {
	if len(topics) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := reconcileEpochMismatch(operationEpoch, s.connEpoch); err != nil {
		return err
	}
	for _, topic := range topics {
		delete(s.observedSubs, topic)
		delete(s.activeSubs, topic)
	}
	return nil
}

func (s *Session) loadManagedSubscriptionHistory(ctx context.Context) error {
	s.mu.Lock()
	if !s.managedRequired || s.managedLoaded {
		s.mu.Unlock()
		return nil
	}
	store := s.managedStore
	identity := s.managedIdentity
	s.mu.Unlock()
	if store == nil || identity == "" {
		return shared.ErrInvalidConfig.WithMessage("mqtt: managed subscription history is required but not configured")
	}
	filters, err := store.List(ctx, identity)
	if err != nil {
		return shared.ErrUnavailable.WithMessage("mqtt: load managed subscription history before broker activation").Wrap(err)
	}
	history := make(map[string]struct{}, len(filters))
	for _, filter := range filters {
		if filter == "" {
			return shared.ErrInvalidConfig.WithMessage("mqtt: managed subscription history contains an empty filter")
		}
		history[filter] = struct{}{}
	}
	s.mu.Lock()
	installed := false
	if !s.managedLoaded {
		s.managedHistory = history
		s.managedLoaded = true
		installed = true
	}
	s.mu.Unlock()
	if installed && s.router != nil {
		// Desired state is not declared until Reconcile. Gate every durable
		// historical filter before broker activation so resumed traffic cannot
		// reach a handler through a stale wildcard/shared subscription.
		s.router.setManagedCleanupFilters(filters)
	}
	return nil
}

func (s *Session) syncManagedCleanupGate(plan connectivity.SessionPlan) {
	if s.router == nil {
		return
	}
	desired := make(map[string]struct{}, len(plan.Subscriptions))
	for _, sub := range plan.Subscriptions {
		desired[sub.Topic] = struct{}{}
	}
	s.mu.Lock()
	if !s.managedRequired {
		s.mu.Unlock()
		s.router.setManagedCleanupFilters(nil)
		return
	}
	filters := make([]string, 0, len(s.managedHistory))
	for filter := range s.managedHistory {
		if _, keep := desired[filter]; !keep {
			filters = append(filters, filter)
		}
	}
	s.mu.Unlock()
	s.router.setManagedCleanupFilters(filters)
}

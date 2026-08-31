package paho

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

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
	if err := s.acquireReload(ctx); err != nil {
		return MapError(err).WithMessage("mqtt: reconcile waiting for session serialization")
	}
	defer s.releaseReload()

	s.mu.Lock()
	if s.terminalErr != nil {
		terminal := s.terminalErr
		s.mu.Unlock()
		return terminal
	}
	recoveryGeneration := uint64(0)
	if s.recoveryAttemptActive {
		recoveryGeneration = s.recoveryGeneration
	}
	s.mu.Unlock()
	return s.reconcileUnderGate(ctx, plan, recoveryGeneration)
}

// reconcileUnderGate converges one plan while its caller owns reloadGate. It
// may call reloadLocked for managed cleanup and never reacquires serialization.
func (s *Session) reconcileUnderGate(
	ctx context.Context,
	plan connectivity.SessionPlan,
	recoveryGeneration uint64,
) (retErr error) {
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
			s.recoveryGeneration == recoveryGeneration &&
			s.recoveryTargetEpoch != 0 &&
			s.recoverySessionPresentEpoch == s.recoveryTargetEpoch &&
			s.connEpoch == s.recoveryTargetEpoch
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
		if retErr != nil && recoveryGeneration == 0 && s.mode == connectivity.SessionExclusive {
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
	// client_id-collision footgun: every replica MUST use a UNIQUE
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
		// reloadGate (and, for exclusive sessions, while the manager still owns
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
	// single owner. Per finding the runtime session manager drives
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

// reconcileTimeout returns the adapter-owned deadline applied to EACH broker
// SUBSCRIBE / UNSUBSCRIBE during reconciliation. A non-positive
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

// connectTimeout returns the deadline applied to the INITIAL connection await
// in Start. A non-positive configured value is coerced to
// DefaultConnectTimeout: zero means "unset", and a negative one that reached a
// hand-built SessionOptions without passing Config.Validate would otherwise
// produce an already-expired context, so every connect attempt would fail
// before it was made.
func (s *Session) connectTimeout() time.Duration {
	if s.opts.ConnectTimeout > 0 {
		return s.opts.ConnectTimeout
	}
	return DefaultConnectTimeout
}

// packetTimeout returns the per-packet acknowledgement budget handed to the
// SDK (CONNACK, SUBACK, UNSUBACK, PUBACK / PUBCOMP).
//
// The SDK applies this budget INSIDE the caller's context, so the effective
// deadline for any packet is the shorter of the two. Its own default is ten
// seconds — shorter than every adapter-owned budget below — which silently
// overrides them: a SUBACK the bridge was willing to wait thirty seconds for is
// abandoned at ten, and the reconcile fails with a deadline error while the
// broker was answering normally.
//
// The budget is therefore the LONGEST enclosing deadline it could pre-empt, so
// the adapter-owned bound is always the one that governs. It is not a liveness
// bound of its own: every packet operation already runs under a deadline of its
// own (reconcile for SUBSCRIBE / UNSUBSCRIBE, the sender budget for PUBLISH,
// the reconnect attempt for CONNECT), which is what actually bounds a wedged
// broker.
func (s *Session) packetTimeout() time.Duration {
	s.mu.Lock()
	publishBudget := s.publishAckBudget
	s.mu.Unlock()
	budget := max(
		s.reconcileTimeout(),
		s.connectTimeout(),
		s.opts.ReconnectTimeout,
		publishBudget,
	)
	// A session built directly, without a Config, carries no sender budget.
	// The documented sender default is what such a sender will use.
	return max(budget, DefaultSenderOptions().Timeout)
}

// notePublishAckBudget raises the session's record of the longest publish
// deadline it serves. It only ever raises: a session shared by several senders
// must not let the shortest of them shorten the SDK's packet budget for the
// rest.
func (s *Session) notePublishAckBudget(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishAckBudget = max(s.publishAckBudget, d)
}

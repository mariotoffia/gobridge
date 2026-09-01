package paho

import (
	"github.com/mariotoffia/gobridge/domain/shared"
)

// permanentQoSDowngradeConfirmations is how many consecutive reconciles must
// conclude the SAME broker grant before the downgrade is treated as permanent.
//
// One weak SUBACK is not proof: a broker can cap QoS transiently while a
// cluster member restarts or an authorization policy is mid-propagation, and
// terminalising on that would turn a blip into a process restart. Repeating the
// identical (filter, requested, granted) verdict is proof — a broker QoS-cap
// policy does not change between reconciles seconds apart.
//
// Three is the smallest count that tolerates a propagation window while still
// stopping the churn within one supervisor backoff ladder.
const permanentQoSDowngradeConfirmations = 3

// qosDowngradeGrant identifies one broker verdict: a filter, the QoS the plan
// requested for it, and the QoS the broker granted. Two reconciles that produce
// an equal value are two confirmations of the same incompatibility; any change
// (a different filter, an operator lowering the requested QoS, a broker
// granting more) starts a fresh count.
type qosDowngradeGrant struct {
	topic     string
	requested byte
	granted   byte
}

// noteQoSDowngrade records one confirmation of a broker grant below the
// requested QoS and returns the error reconcile must surface.
//
// A downgrade leaves the filter inactive, so readiness stays below Full and the
// session manager treats the reconcile failure as a session failure — which
// restarts the session. Without a confirmation count that restart hits the same
// broker policy and fails identically, forever: a persistent session loops at
// the supervisor's backoff cap, and an exclusive one additionally releases and
// re-seizes its lease each cycle, resetting every standby's observation window.
//
// Once the same grant has been confirmed permanentQoSDowngradeConfirmations
// times the returned error carries shared.ErrTransportClosedPermanently, which
// runtime/session escalates to a terminal restart. The bridge stops retrying a
// configuration only a human can fix — lower the route's QoS, or lift the
// broker's cap.
//
// A confirmation is not always a fresh broker verdict, and that is deliberate.
// An unchanged downgraded filter is NOT re-subscribed (reconcileApply keeps the
// requested QoS as its delta baseline), so on a session that stays connected —
// the non-exclusive restart path — every reconcile after the first re-reads the
// standing observed grant. Nothing short of a reconnect can retest it, so
// counting only fresh SUBACKs would never terminate that loop. Escalating is
// what produces the retest: the process restart builds a fresh session that
// SUBSCRIBEs again, so a genuinely transient cap recovers on the next start
// while a broker policy surfaces as a crash-loop an operator can see. An
// exclusive session disconnects on every failed reconcile, so its
// confirmations are three independent broker verdicts.
func (s *Session) noteQoSDowngrade(grant qosDowngradeGrant) *shared.BridgeError {
	err := qosDowngradeError(grant.topic, grant.requested, grant.granted)

	s.mu.Lock()
	if s.qosDowngradeConfirmed != grant {
		s.qosDowngradeConfirmed = grant
		s.qosDowngradeStreak = 0
	}
	s.qosDowngradeStreak++
	streak := s.qosDowngradeStreak
	s.mu.Unlock()

	if streak < permanentQoSDowngradeConfirmations {
		return err
	}
	err = err.With("confirmations", streak).
		Wrap(shared.ErrTransportClosedPermanently)
	if streak == permanentQoSDowngradeConfirmations && s.logger != nil {
		s.logger.Warn("mqtt: broker QoS grant is permanently incompatible with the route; "+
			"the same downgrade was confirmed on every reconcile, so the session is failed "+
			"terminally instead of restarting into it forever — lower the route's QoS to the "+
			"granted level or lift the broker's QoS cap",
			"client_id", s.opts.ClientID,
			"topic", grant.topic,
			"requested_qos", grant.requested,
			"granted_qos", grant.granted,
			"confirmations", streak,
		)
	}
	return err
}

// clearQoSDowngrade forgets the confirmation streak after a reconcile that
// converged with no downgrade. The streak counts CONSECUTIVE confirmations of
// one incompatibility, so a broker (or a lowered route QoS) that satisfies the
// request restores the full retry budget for any later, unrelated downgrade.
func (s *Session) clearQoSDowngrade() {
	s.mu.Lock()
	s.qosDowngradeConfirmed = qosDowngradeGrant{}
	s.qosDowngradeStreak = 0
	s.mu.Unlock()
}

// qosDowngradeError builds the classified rejection a downgraded grant returns.
func qosDowngradeError(topic string, requested, granted byte) *shared.BridgeError {
	return shared.ErrQoSNotSupported.
		WithMessage("mqtt: broker granted subscription QoS below requested").
		With("topic", topic).
		With("requested_qos", int(requested)).
		With("granted_qos", int(granted))
}

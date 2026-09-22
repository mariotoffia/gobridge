package paho

import (
	"fmt"
	"sort"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A broker may grant a subscription a lower QoS than requested (MQTT 5
// SUBACK). One weak SUBACK can be transient — a cluster node restarting, an
// authorization rule mid-propagation — so the bridge first confirms it with
// fresh SUBSCRIBEs, qosDowngradeConfirmInterval apart. When
// qosDowngradeConfirmations fresh SUBACKs agree, the grant is ACCEPTED: the
// subscription stays active at the granted QoS as best effort (at QoS 0 no
// acknowledgement and no redelivery; messages published while the bridge is
// disconnected may be lost) and is re-checked every qos_recheck_interval.
//
// A lower grant never stops the session or the process. A broker that REFUSES
// a subscription degrades only its own session; a broker that accepts it at a
// lower QoS gave the milder answer and must not be punished harder.

// qosDowngradeConfirmations is how many fresh SUBACKs must report the same
// lower grant before it is accepted as best effort.
const qosDowngradeConfirmations = 3

// qosDowngradeConfirmInterval spaces the confirmation SUBSCRIBEs, long enough
// to ride out a brief broker-side cap.
const qosDowngradeConfirmInterval = 5 * time.Second

// qosDowngrade is the record of one filter the broker granted below the
// requested QoS.
type qosDowngrade struct {
	requested     byte
	granted       byte
	confirmations int           // fresh SUBACKs that reported this grant
	acceptedAt    time.Time     // zero while confirming
	recheck       time.Duration // re-check interval once accepted; 0 = off
	due           time.Time     // next probe SUBSCRIBE; zero = none scheduled
	// noVerdictRounds counts consecutive probes that got no grant; a grant
	// resets it. The first of a streak is warned about.
	noVerdictRounds int
}

func (d *qosDowngrade) accepted() bool { return !d.acceptedAt.IsZero() }

// retryInterval is how long a probe that got no verdict waits to try again. An
// accepted downgrade keeps its re-check cadence. One still confirming backs
// off: each consecutive round without a verdict doubles the wait, from
// qosDowngradeConfirmInterval up to MinQoSRecheckInterval, so a broker that
// keeps refusing the probe is not asked every few seconds forever.
func (d *qosDowngrade) retryInterval() time.Duration {
	if d.accepted() {
		return d.recheck
	}
	delay := qosDowngradeConfirmInterval
	for round := 1; round < d.noVerdictRounds && delay < MinQoSRecheckInterval; round++ {
		delay *= 2
	}
	return min(delay, MinQoSRecheckInterval)
}

// grantVerdict is what one fresh SUBACK grant changed.
type grantVerdict uint8

const (
	grantUnchanged  grantVerdict = iota
	grantDowngraded              // a newly reported lower grant; confirmation starts
	grantAccepted                // the confirmed lower grant is accepted as best effort
	grantRecovered               // an accepted downgrade is granted the requested QoS again
)

// grantReport carries one verdict out of s.mu so it is logged without the lock.
type grantReport struct {
	topic     string
	requested byte
	granted   byte
	verdict   grantVerdict
}

// applyGrantLocked records one FRESH broker grant for topic — a SUBACK to a
// SUBSCRIBE this session just sent — in the observed, active and downgrade
// state. It is the only writer of s.qosDowngrades besides pruning, so reconcile
// and the probe agree on what a grant means. Callers hold s.mu and have checked
// the connection epoch.
func (s *Session) applyGrantLocked(topic string, requested, granted byte, recheck time.Duration) grantVerdict {
	if s.observedSubs == nil {
		s.observedSubs = make(map[string]subscriptionGrant)
	}
	if s.activeSubs == nil {
		s.activeSubs = make(map[string]byte)
	}
	s.observedSubs[topic] = subscriptionGrant{Requested: requested, Granted: granted}
	d := s.qosDowngrades[topic]
	if granted >= requested {
		s.activeSubs[topic] = granted
		delete(s.qosDowngrades, topic)
		if d != nil && d.accepted() {
			return grantRecovered
		}
		// A grant that recovers while still confirming was never accepted.
		return grantUnchanged
	}

	now := s.clock().Now()
	verdict := grantUnchanged
	if d == nil || d.requested != requested || d.granted != granted {
		// A different grant is a new broker answer: it restarts confirmation.
		d = &qosDowngrade{requested: requested, granted: granted}
		if s.qosDowngrades == nil {
			s.qosDowngrades = make(map[string]*qosDowngrade)
		}
		s.qosDowngrades[topic] = d
		verdict = grantDowngraded
	}
	d.recheck = recheck
	d.noVerdictRounds = 0
	if !d.accepted() {
		d.confirmations++
		if d.confirmations >= qosDowngradeConfirmations {
			d.acceptedAt = now
			verdict = grantAccepted
		}
	}
	if !d.accepted() {
		delete(s.activeSubs, topic)
		d.due = now.Add(qosDowngradeConfirmInterval)
		return verdict
	}
	s.activeSubs[topic] = granted
	d.due = time.Time{}
	if d.recheck > 0 {
		d.due = now.Add(d.recheck)
	}
	return verdict
}

// dropUnwantedQoSDowngradesLocked forgets every downgrade whose filter the plan
// does not want at the recorded requested QoS. Reconcile calls it when it
// stashes a new plan, before any broker operation, so neither a reconcile that
// fails nor the empty-plan no-op leaves the gauge and health reporting a filter
// that is gone. Callers hold s.mu.
func (s *Session) dropUnwantedQoSDowngradesLocked(desired map[string]byte) {
	for topic, d := range s.qosDowngrades {
		if requested, ok := desired[topic]; !ok || requested != d.requested {
			delete(s.qosDowngrades, topic)
		}
	}
}

// reconcileQoSDowngradesLocked brings the downgrade records in line with the
// plan a reconcile just applied: it forgets filters the plan no longer wants at
// the recorded requested QoS, and applies a changed qos_recheck_interval to an
// accepted downgrade without waiting for its next SUBACK (an unchanged filter
// is not re-subscribed, so no SUBACK would carry it). Callers hold s.mu.
func (s *Session) reconcileQoSDowngradesLocked(desired map[string]byte, recheck map[string]time.Duration) {
	s.dropUnwantedQoSDowngradesLocked(desired)
	now := s.clock().Now()
	for topic, d := range s.qosDowngrades {
		if interval := recheck[topic]; interval != d.recheck {
			d.recheck = interval
			if d.accepted() {
				d.due = time.Time{}
				if interval > 0 {
					d.due = now.Add(interval)
				}
			}
		}
	}
}

// syncQoSDowngradeGaugeLocked emits MQTTQoSDowngradedActive when the number of
// accepted downgrades changed, so a change is visible at once; Health re-emits
// the count on every sweep. Callers hold s.mu.
func (s *Session) syncQoSDowngradeGaugeLocked() {
	accepted := 0
	for _, d := range s.qosDowngrades {
		if d.accepted() {
			accepted++
		}
	}
	if accepted == s.qosDowngradeGauge {
		return
	}
	s.qosDowngradeGauge = accepted
	s.metrics.Gauge(MetricMQTTQoSDowngradedActive, float64(accepted),
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
}

// retireQoSDowngradesLocked drops every downgrade and its probe when the
// session closes, so the gauge does not report a subscription that is gone.
// Callers hold s.mu.
func (s *Session) retireQoSDowngradesLocked() {
	if s.qosProbeCancel != nil {
		s.qosProbeCancel()
		s.qosProbeCancel = nil
	}
	clear(s.qosDowngrades)
	s.syncQoSDowngradeGaugeLocked()
}

// qosDowngradeHealthLocked returns the sorted filters accepted as best effort
// that are contract-active — a reconnect deactivates them until its reconcile
// re-subscribes — the number of accepted downgrades (the
// MQTTQoSDowngradedActive value), and whether any downgrade is still
// confirming. Callers hold s.mu.
func (s *Session) qosDowngradeHealthLocked() (bestEffort []string, accepted int, confirming bool) {
	for topic, d := range s.qosDowngrades {
		if !d.accepted() {
			confirming = true
			continue
		}
		accepted++
		if _, active := s.activeSubs[topic]; active {
			bestEffort = append(bestEffort, topic)
		}
	}
	sort.Strings(bestEffort)
	return bestEffort, accepted, confirming
}

// reportGrants logs and counts verdicts after s.mu is released.
func (s *Session) reportGrants(reports []grantReport) {
	tag := shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID}
	for _, r := range reports {
		if r.verdict == grantDowngraded {
			s.metrics.Counter(MetricMQTTQoSDowngraded, 1, tag)
		}
		if s.logger == nil {
			continue
		}
		attrs := []any{
			"client_id", s.opts.ClientID,
			"topic", r.topic,
			"requested_qos", r.requested,
			"granted_qos", r.granted,
		}
		switch r.verdict {
		case grantDowngraded:
			s.logger.Warn("mqtt: broker downgraded subscription QoS below requested; "+
				"confirming with fresh SUBSCRIBEs before accepting it as best effort", attrs...)
		case grantAccepted:
			consequence := "delivery runs at the granted QoS instead of the requested one"
			advice := "Lower the route's qos to the granted level, or lift the broker's cap"
			if r.granted == 0 {
				// The retry check exempts a route whose subscriptions all requested QoS
				// 1 or 2 on a resuming session, trusting broker redelivery. Say where a
				// failed delivery goes now that the broker cannot redeliver it.
				consequence = "at QoS 0 there is no acknowledgement or redelivery, and messages " +
					"published while the bridge is disconnected may be lost. A delivery the route " +
					"fails to process cannot be redelivered by the broker either: it goes to the " +
					"DLQ or, when no DLQ store is configured, is dropped and counted as " +
					"retry_unsupported, even though the route was validated for the QoS it requested"
				advice = "Configure a DLQ store, lower the route's qos to the granted level, " +
					"or lift the broker's cap"
			}
			s.logger.Error("mqtt: broker keeps granting subscription QoS below requested; the "+
				"subscription is kept active as best effort — "+consequence+". "+advice, attrs...)
		case grantRecovered:
			s.logger.Info("mqtt: broker grants the requested subscription QoS again; "+
				"the best-effort downgrade is cleared", attrs...)
		}
	}
}

// planDesiredQoS returns the highest requested QoS per filter of plan. A
// subscription whose QoS is outside 0..2 is not desired at all: the range is
// checked before the byte conversion, which would alias 257 to 1 (see
// ValidateMQTTSubscription). Such a plan is stashed before reconcile rejects
// it, so aliasing would keep a downgrade record — and the probe that reads the
// stashed plan — alive for a subscription nothing may request.
func planDesiredQoS(plan *connectivity.SessionPlan) map[string]byte {
	desired := make(map[string]byte)
	if plan == nil {
		return desired
	}
	for _, sub := range plan.Subscriptions {
		if sub.QoS < 0 || sub.QoS > maxMQTTQoS {
			continue
		}
		qos := byte(sub.QoS)
		if current, ok := desired[sub.Topic]; !ok || qos > current {
			desired[sub.Topic] = qos
		}
	}
	return desired
}

// subscriptionQoSRecheckInterval returns the qos_recheck_interval a plan
// subscription carries. A nil config (a plan built in code) uses
// DefaultQoSRecheckInterval. Anything else must be the MQTT plugin config: a
// typed-nil, foreign or non-PluginConfig value fails closed, as it does at the
// factory seams, instead of passing for an omitted option.
func subscriptionQoSRecheckInterval(cfg any) (time.Duration, error) {
	if cfg == nil {
		return DefaultQoSRecheckInterval, nil
	}
	pc, _ := cfg.(ports.PluginConfig) // a non-PluginConfig stays nil and configFromSpec refuses it
	c, err := configFromSpec(pc)
	if err != nil {
		return 0, shared.ErrInvalidConfig.Wrap(err).WithMessage(
			fmt.Sprintf("mqtt: subscription config must be a non-nil MQTT plugin config, got %T", cfg))
	}
	if err := c.Subscription.validate(); err != nil {
		return 0, err
	}
	return c.Subscription.QoSRecheckInterval, nil
}

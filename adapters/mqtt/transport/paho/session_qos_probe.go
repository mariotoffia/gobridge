package paho

import (
	"context"
	"sort"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
)

// armQoSProbeLocked schedules one probe at the earliest due downgrade and
// cancels any earlier schedule. Every state change re-arms, so exactly one
// schedule is live. Callers hold s.mu.
func (s *Session) armQoSProbeLocked() {
	if s.qosProbeCancel != nil {
		s.qosProbeCancel()
		s.qosProbeCancel = nil
	}
	if s.closed {
		return
	}
	var next time.Time
	for _, d := range s.qosDowngrades {
		if !d.due.IsZero() && (next.IsZero() || d.due.Before(next)) {
			next = d.due
		}
	}
	if next.IsZero() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.qosProbeCancel = cancel
	timer := s.clock().NewTimer(next.Sub(s.clock().Now()))
	go s.awaitQoSProbe(ctx, timer, s.closedCh)
}

func (s *Session) awaitQoSProbe(ctx context.Context, timer clock.Timer, closedCh <-chan struct{}) {
	defer timer.Stop()
	select {
	case <-timer.C():
	case <-ctx.Done():
		return
	case <-closedCh:
		return
	}
	s.probeQoSDowngrades(ctx)
}

// probeQoSDowngrades re-sends SUBSCRIBE for every downgrade whose confirmation
// or re-check is due, and records the fresh grants.
//
// It is on the session's reconcile path: it owns reloadGate for the whole
// round-trip, so it never interleaves with a reconcile, a reload, a recovery or
// a managed-subscription cleanup, and it acts only on the connection generation
// whose SUBACK recorded the downgrade. It is deliberately NOT a reconcile: it
// reports nothing to the session manager and never disconnects, so a failed
// probe cannot release an exclusive lease. Retain handling is 1 in every
// session mode — the subscription already exists, so the broker must not replay
// retained messages for a re-check.
func (s *Session) probeQoSDowngrades(ctx context.Context) {
	if err := s.acquireReload(ctx); err != nil {
		return // a newer schedule or Close replaced this probe
	}
	defer s.releaseReload()

	s.mu.Lock()
	if ctx.Err() != nil || s.closed || s.terminalErr != nil || s.cm == nil || !s.connected {
		// Disconnected: the reconnect's reconcile SUBSCRIBEs afresh and re-arms.
		s.mu.Unlock()
		return
	}
	now := s.clock().Now()
	desired := planDesiredQoS(s.plan)
	var specs []subscribeSpec
	for topic, d := range s.qosDowngrades {
		if d.due.IsZero() || d.due.After(now) {
			continue
		}
		grant := s.observedSubs[topic]
		if desired[topic] != d.requested || grant != (subscriptionGrant{Requested: d.requested, Granted: d.granted}) {
			// The plan or the connection moved on; the next reconcile decides.
			d.due = time.Time{}
			continue
		}
		specs = append(specs, s.subscribeSpec(topic, d.requested, 1))
	}
	if len(specs) == 0 {
		s.armQoSProbeLocked()
		s.mu.Unlock()
		return
	}
	cm, epoch := s.cm, s.connEpoch
	s.mu.Unlock()
	sort.Slice(specs, func(i, j int) bool { return specs[i].Topic < specs[j].Topic })

	subCtx, cancel := context.WithTimeout(ctx, s.reconcileTimeout())
	reasons, subErr := cm.Subscribe(subCtx, specs)
	cancel()
	succeeded, refusal, refusedTopic := classifySubackReasons(specs, reasons)

	s.mu.Lock()
	if s.closed || s.connEpoch != epoch {
		s.mu.Unlock()
		return
	}
	now = s.clock().Now()
	granted := make(map[string]byte, len(succeeded))
	for _, opt := range succeeded {
		granted[opt.Topic] = opt.QoS
	}
	var reports []grantReport
	warn := false
	for _, spec := range specs {
		d := s.qosDowngrades[spec.Topic]
		if d == nil {
			continue
		}
		qos, ok := granted[spec.Topic]
		if !ok {
			// No verdict (refused, short SUBACK, or no SUBACK at all): keep the
			// recorded grant and ask again later. A broker that keeps refusing
			// is warned about once per streak, not on every retry.
			d.due = now.Add(d.retryInterval())
			warn = warn || !d.noVerdict
			d.noVerdict = true
			continue
		}
		if v := s.applyGrantLocked(spec.Topic, d.requested, qos, d.recheck); v != grantUnchanged {
			reports = append(reports, grantReport{topic: spec.Topic, requested: spec.QoS, granted: qos, verdict: v})
		}
	}
	s.syncQoSDowngradeGaugeLocked()
	s.armQoSProbeLocked()
	s.mu.Unlock()

	s.reportGrants(reports)
	if warn && s.logger != nil {
		// No reason codes at all: the SDK error is the only evidence, and
		// classifySubackReasons would call it a short SUBACK.
		cause := error(refusal)
		if subErr != nil && len(reasons) == 0 {
			cause = MapError(subErr)
		}
		s.logger.Warn("mqtt: QoS downgrade re-check SUBSCRIBE got no grant; keeping the last grant and retrying",
			"client_id", s.opts.ClientID, "topic", refusedTopic, "error", cause)
	}
}

// subscribeSpec builds the SUBSCRIBE options for one filter. No-Local follows
// the session's no_local setting except on a shared subscription, where it is
// an MQTT 5 protocol error.
func (s *Session) subscribeSpec(topic string, qos, retainHandling byte) subscribeSpec {
	return subscribeSpec{
		Topic:          topic,
		QoS:            qos,
		NoLocal:        s.opts.NoLocal && !isSharedSubscriptionFilter(topic),
		RetainHandling: retainHandling,
	}
}

package paho

import (
	"context"
	"errors"
	"sort"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Health returns the current health state of the session, including
// subscription and handler readiness.
//
// Ready reports CONNECTIVITY ONLY: it is true when the session is
// connected to the broker. This is intentional and matches the
// ports.SessionHealth contract — Ready does NOT imply that subscriptions
// are active or that receiver handlers are registered. A sender-only
// session is fully serviceable as soon as it is connected, so a liveness
// probe keyed on Ready is correct for it.
//
// For a RECEIVER session, connectivity alone is not operational
// readiness: messages are dropped until subscriptions are reconciled and
// a handler is registered. Production READINESS probes for receiver
// sessions must therefore gate on ServiceLevel == Full (which the monitor
// exposes via ?level=full), not on Ready. ServiceLevel describes
// operational completeness:
//   - Full: the latest explicit plan has successfully converged exactly (no
//     missing or stale broker subscriptions), every unique desired topic filter
//     is active at or above requested QoS, every expected receiver handler is
//     registered, and no publishes are
//     still buffered waiting for a handler (A-5: a covered
//     subscription whose receiver died keeps its messages retained in the
//     pending buffer, so a non-empty buffer degrades readiness even while a
//     surviving receiver keeps the session-total handler count above zero)
//   - Degraded: connected but not all desired subscriptions are active
//   - None: not connected, or no subscriptions/handlers registered
//
// For sender-only sessions (no subscriptions), ServiceLevel is Full when connected.
func (s *Session) Health(_ context.Context) ports.SessionHealth {
	s.mu.Lock()
	cm := s.cm
	desired := make(map[string]byte)
	var expectedReceiverIDs []string
	planDeclared := s.plan != nil
	// A reserved ingress receiver (Factory.NewReceiver) that has not yet declared
	// its first plan is a RECEIVER whose Reconcile is still pending — not a
	// sender-only session — so it must not be reported Full before it subscribes
	// (MQTT-OBS-2).
	ingressReserved := s.ingressReceiverReserved
	if planDeclared {
		for _, sub := range s.plan.Subscriptions {
			qos := byte(sub.QoS)
			if current, ok := desired[sub.Topic]; !ok || qos > current {
				desired[sub.Topic] = qos
			}
		}
		expectedReceiverIDs = append(expectedReceiverIDs, s.plan.ExpectedReceiverIDs...)
	}
	active := make(map[string]byte, len(s.activeSubs))
	topics := make([]string, 0, len(s.activeSubs))
	for topic, qos := range s.activeSubs {
		active[topic] = qos
		topics = append(topics, topic)
	}
	connected := cm != nil && s.connected
	latchedSubscriptionsSatisfied := s.subscriptionsSatisfied
	recoveryPending := s.recoveryPending
	recoveryErr := s.recoveryErr
	terminalErr := s.terminalErr
	recoveryRecycleCount := s.recoveryRecycleCount
	s.mu.Unlock()
	sort.Strings(topics)

	handlerIDs := s.router.HandlerIDs()
	handlerCount := len(handlerIDs)
	pendingCount := s.router.PendingCount()
	wantedCount := len(desired)
	activeCount := len(active)
	subscriptionsSatisfied := latchedSubscriptionsSatisfied
	if !planDeclared {
		// Compatibility for zero-value/programmatic sessions that have not
		// declared an explicit plan through Reconcile.
		subscriptionsSatisfied = desiredSubscriptionsActive(desired, active)
	}
	handlersSatisfied := expectedHandlersRegistered(expectedReceiverIDs, handlerIDs)
	if len(expectedReceiverIDs) == 0 && wantedCount > 0 {
		// Plans assembled before ExpectedReceiverIDs was added retain the legacy
		// contract: at least one handler proves receiver-side registration.
		handlersSatisfied = handlerCount > 0
	}

	var sl ports.ServiceLevel
	switch {
	case !connected || terminalErr != nil:
		sl = ports.ServiceLevelNone
	case recoveryPending:
		sl = ports.ServiceLevelDegraded
	case ingressReserved && !planDeclared:
		// Reserved ingress receiver, first Reconcile still pending: connected but
		// not yet subscribed, so cap below Full so a ?level=full readiness probe
		// cannot pass during the pre-first-Reconcile window (MQTT-OBS-2). The
		// sender-only Full branch below must not claim this receiver.
		sl = ports.ServiceLevelDegraded
	case wantedCount == 0 && len(expectedReceiverIDs) == 0 && (!planDeclared || subscriptionsSatisfied) && !recoveryPending:
		// Sender-only session: no subscriptions or receiver handlers expected.
		// An explicit empty plan is Full only after stale removals converge.
		sl = ports.ServiceLevelFull
	case subscriptionsSatisfied && handlersSatisfied && pendingCount == 0 && !recoveryPending:
		sl = ports.ServiceLevelFull
	case activeCount == 0 && handlerCount == 0:
		sl = ports.ServiceLevelNone
	default:
		sl = ports.ServiceLevelDegraded
	}

	rm := s.opts.ReceiveMaximum
	if rm == 0 {
		rm = DefaultReceiveMaximum
	}
	unsettled := s.router.unsettledSnapshot(rm)
	tags := []shared.Tag{{Key: shared.TagKeySessionID, Value: s.opts.ClientID}}
	s.metrics.Gauge(MetricMQTTUnsettled, float64(unsettled.Count), tags...)
	s.metrics.Gauge(MetricMQTTOldestUnsettledAge, unsettled.OldestAge.Seconds(), tags...)
	s.metrics.Gauge(MetricMQTTReceiveWindowUtilization, unsettled.ReceiveWindowUtilization, tags...)

	return ports.SessionHealth{
		Connected:                connected,
		LastError:                errors.Join(recoveryErr, terminalErr),
		SubscriptionsWanted:      wantedCount,
		SubscriptionsActive:      activeCount,
		SubscriptionsSatisfied:   &subscriptionsSatisfied,
		HandlersRegistered:       handlerCount,
		ReceiveMaximum:           rm,
		UnsettledCount:           unsettled.Count,
		OldestUnsettledAge:       unsettled.OldestAge,
		ReceiveWindowUtilization: unsettled.ReceiveWindowUtilization,
		RecoveryRecycleCount:     recoveryRecycleCount,
		Ready:                    connected && terminalErr == nil,
		ServiceLevel:             sl,
		ActiveTopics:             topics,
	}
}

func desiredSubscriptionsActive(desired, active map[string]byte) bool {
	if len(desired) != len(active) {
		return false
	}
	for topic, requestedQoS := range desired {
		actualQoS, ok := active[topic]
		if !ok || actualQoS < requestedQoS {
			return false
		}
	}
	return true
}

func expectedHandlersRegistered(expected, registered []string) bool {
	if len(expected) == 0 {
		return true
	}
	registeredSet := make(map[string]struct{}, len(registered))
	for _, id := range registered {
		registeredSet[id] = struct{}{}
	}
	for _, id := range expected {
		if _, ok := registeredSet[id]; !ok {
			return false
		}
	}
	return true
}

// Events returns the channel on which session lifecycle events are emitted.
//
// Read under s.mu: the F-1 Reload-failure re-Start reassigns s.events (it
// re-materialises a fresh channel after the terminal-death close), so an
// unlocked read could race that write under the race detector. The runtime
// manager re-invokes Events() at the top of each handleEvents (once per Run,
// after Start), so it always observes the current channel.
func (s *Session) Events() <-chan ports.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

func (s *Session) pushEvent(t ports.SessionEventType, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.eventsClosed {
		// s.closed: terminal shutdown. s.eventsClosed: a Reload-failure
		// already closed s.events to signal terminal death (F-1) and a
		// re-Start has not yet re-materialised it. Both are checked under
		// s.mu that also guards close(s.events), so no send can race the
		// close.
		return
	}

	ev := ports.SessionEvent{Type: t, Err: err, Timestamp: s.clock().Now()}
	select {
	case s.events <- ev:
	default:
		// Channel full: drop the OLDEST event to make room for this one.
		// This drop-oldest eviction is the common back-pressure path under an
		// event storm (F-6): it can evict an unconsumed SessionConnected before
		// the manager reads it, so a reconcile that would have run on that
		// connect edge is deferred to the NEXT connect edge. Bounded (the
		// session reconnects and re-emits) and drop-oldest by design — not
		// hardened (non-evictable / coalesce-by-type) on purpose.
		//
		// Every actually-lost event increments MetricMQTTEventDropped exactly
		// once: the evicted oldest here (B-2/D-2 — previously this drop was
		// silent and only the impossible double-failure below was metered), and
		// the new event below if it still cannot be enqueued. Alert on
		// MetricMQTTEventDropped if it is non-zero in steady state.
		select {
		case <-s.events:
			s.metrics.Counter(MetricMQTTEventDropped, 1)
		default:
		}
		select {
		case s.events <- ev:
		default:
			// Still full after evicting the oldest — only reachable if a
			// producer we do not serialise under s.mu refilled the slot; the
			// new event is lost, so count it too.
			s.metrics.Counter(MetricMQTTEventDropped, 1)
		}
	}
}

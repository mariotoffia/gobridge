package paho

import (
	"context"

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
//   - Full: all desired subscriptions active and handlers registered, and no
//     publishes are still buffered waiting for a handler (A-5: a covered
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
	// Capture wantedCount UNDER the lock (B-4): s.plan is swapped by pointer on
	// Reconcile, so dereferencing a captured pointer after unlocking could race
	// that swap under the race detector. Read the length while holding s.mu.
	wantedCount := 0
	if s.plan != nil {
		wantedCount = len(s.plan.Subscriptions)
	}
	activeCount := len(s.activeSubs)
	connected := cm != nil && s.connected
	topics := make([]string, 0, len(s.activeSubs))
	for t := range s.activeSubs {
		topics = append(topics, t)
	}
	s.mu.Unlock()
	handlerCount := s.router.HandlerCount()
	// pendingCount is the router's pre-registration / covered-retained backlog.
	// In steady state (every subscription's handler registered) an incoming
	// publish dispatches immediately and never enters the buffer, so this is 0;
	// a non-zero value means some covered subscription still lacks a live
	// handler (A-5).
	pendingCount := s.router.PendingCount()

	var sl ports.ServiceLevel
	switch {
	case !connected:
		sl = ports.ServiceLevelNone
	case wantedCount == 0:
		// Sender-only session: no subscriptions expected.
		sl = ports.ServiceLevelFull
	case activeCount == wantedCount && handlerCount > 0 && pendingCount == 0:
		sl = ports.ServiceLevelFull
	case activeCount == 0 && handlerCount == 0:
		sl = ports.ServiceLevelNone
	default:
		sl = ports.ServiceLevelDegraded
	}

	rm := s.opts.ReceiveMaximum
	if rm == 0 {
		// NewSession coerces a zero ReceiveMaximum to DefaultReceiveMaximum,
		// so s.opts.ReceiveMaximum is normally non-zero here. This fallback
		// only covers a Session built by hand (bypassing NewSession) and
		// reports the same effective default the CONNECT would carry.
		rm = DefaultReceiveMaximum
	}

	return ports.SessionHealth{
		Connected:           connected,
		SubscriptionsWanted: wantedCount,
		SubscriptionsActive: activeCount,
		HandlersRegistered:  handlerCount,
		ReceiveMaximum:      rm,
		Ready:               connected,
		ServiceLevel:        sl,
		ActiveTopics:        topics,
	}
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

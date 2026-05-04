package paho

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Health returns the current health state of the session, including
// subscription and handler readiness.
//
// Ready is true when the session is connected to the broker (connectivity only).
//
// ServiceLevel describes operational completeness:
//   - Full: all desired subscriptions active and handlers registered (when expected)
//   - Degraded: connected but not all desired subscriptions are active
//   - None: not connected, or no subscriptions/handlers registered
//
// For sender-only sessions (no subscriptions), ServiceLevel is Full when connected.
func (s *Session) Health(_ context.Context) ports.SessionHealth {
	s.mu.Lock()
	cm := s.cm
	plan := s.plan
	activeCount := len(s.activeSubs)
	connected := cm != nil && s.connected
	topics := make([]string, 0, len(s.activeSubs))
	for t := range s.activeSubs {
		topics = append(topics, t)
	}
	s.mu.Unlock()
	wantedCount := 0
	if plan != nil {
		wantedCount = len(plan.Subscriptions)
	}
	handlerCount := s.router.HandlerCount()

	var sl ports.ServiceLevel
	switch {
	case !connected:
		sl = ports.ServiceLevelNone
	case wantedCount == 0:
		// Sender-only session: no subscriptions expected.
		sl = ports.ServiceLevelFull
	case activeCount == wantedCount && handlerCount > 0:
		sl = ports.ServiceLevelFull
	case activeCount == 0 && handlerCount == 0:
		sl = ports.ServiceLevelNone
	default:
		sl = ports.ServiceLevelDegraded
	}

	rm := s.opts.ReceiveMaximum
	if rm == 0 {
		rm = 65535 // MQTT v5 default when not explicitly configured
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
func (s *Session) Events() <-chan ports.SessionEvent {
	return s.events
}

func (s *Session) pushEvent(t ports.SessionEventType, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}

	ev := ports.SessionEvent{Type: t, Err: err, Timestamp: s.clock().Now()}
	select {
	case s.events <- ev:
	default:
		// Drop oldest event to make room.
		select {
		case <-s.events:
		default:
		}
		select {
		case s.events <- ev:
		default:
			// Event channel still full after draining oldest; event lost.
			s.metrics.Counter(domain.MetricMQTTEventDropped, 1)
		}
	}
}

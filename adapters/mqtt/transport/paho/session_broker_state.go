package paho

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// EndBrokerStateOnClose implements ports.BrokerStateEnder (ADR 0024). The next
// Close of a persistent or exclusive session that is connected at that moment
// ends its broker session after its own connection has stopped, with one short
// clean-start connection as its client ID (endBrokerSession). An ephemeral
// session keeps no broker state, and a session that is not connected never
// connected as the identity, or lost the connection and is reconnecting, so
// neither ends anything.
func (s *Session) EndBrokerStateOnClose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endBrokerStateOnClose = true
}

// endsBrokerState reports whether a session in mode keeps state on the broker
// after it disconnects.
func endsBrokerState(mode connectivity.SessionMode) bool {
	return mode == connectivity.SessionPersistent || mode == connectivity.SessionExclusive
}

// endBrokerStateAfterClose ends the broker session Close was asked to end.
// disconnErr is the error of Close's own disconnect: while it is set, autopaho
// may still be connected or reconnecting as the client ID, so nothing is sent
// and the broker session is left as it is. A failure is logged at Warn (the
// factory scopes the logger with session_id) and counted on
// shared.MetricBrokerStateEndFailures; it never fails the Close, because the
// reload that asked continues either way.
func (s *Session) endBrokerStateAfterClose(ctx context.Context, disconnErr error) {
	err := disconnErr
	if err != nil {
		err = fmt.Errorf("the session's own connection did not stop: %w", err)
	} else {
		end := s.endBrokerSession
		if s.endBrokerSessionOverride != nil {
			end = s.endBrokerSessionOverride
		}
		err = end(ctx)
	}
	if err != nil {
		s.metrics.Counter(shared.MetricBrokerStateEndFailures, 1,
			shared.Tag{Key: shared.TagKeySessionID, Value: s.metricSessionID()})
		if s.logger != nil {
			s.logger.Warn("mqtt: could not end the broker session the next configuration no longer has; "+
				"the broker keeps the session, its subscriptions and its queued messages until it expires "+
				"the session, which an MQTT 3.1.1 broker may never do",
				"client_id", s.opts.ClientID, "error", err)
		}
		return
	}
	if s.logger != nil {
		s.logger.Info("mqtt: ended the broker session the next configuration no longer has",
			"client_id", s.opts.ClientID)
	}
}

// metricSessionID is the session_id tag value of this session's metrics: the
// GoBridge session_id, or the client ID for a session built without the
// factory.
func (s *Session) metricSessionID() string {
	if s.sessionID != "" {
		return s.sessionID
	}
	return s.opts.ClientID
}

var _ ports.BrokerStateEnder = (*Session)(nil)

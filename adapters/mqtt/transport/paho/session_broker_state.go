package paho

import (
	"context"
	"fmt"
	"time"

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
// neither ends anything. A non-zero before bounds the clean-start connection.
func (s *Session) EndBrokerStateOnClose(before time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endBrokerStateOnClose = true
	s.endBrokerStateBefore = before
}

// endsBrokerState reports whether a session in mode keeps state on the broker
// after it disconnects.
func endsBrokerState(mode connectivity.SessionMode) bool {
	return mode == connectivity.SessionPersistent || mode == connectivity.SessionExclusive
}

// endBrokerStateAfterClose ends the broker session Close was asked to end.
// disconnErr is the error of Close's own disconnect: while it is set, autopaho
// may still be connected or reconnecting as the client ID, so nothing is sent
// and the broker session is left as it is. A non-zero before is the local
// lease deadline the clean-start connection must finish by; past it another
// instance may be connected as the client ID, so once it passed nothing is
// sent. A failure is logged at Warn (the factory scopes the logger with
// session_id) and counted on shared.MetricBrokerStateEndFailures; it never
// fails the Close, because the reload that asked continues either way.
func (s *Session) endBrokerStateAfterClose(ctx context.Context, disconnErr error, before time.Time) {
	if !before.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, before)
		defer cancel()
	}
	var err error
	switch {
	case disconnErr != nil:
		err = fmt.Errorf("the session's own connection did not stop: %w", disconnErr)
	case ctx.Err() != nil:
		err = fmt.Errorf("no time left to connect before the close or lease deadline: %w", ctx.Err())
	default:
		err = s.callEndBrokerSession(ctx)
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

// startFreshBrokerSession ends the broker session the client ID may still have
// before the first connection of a session whose broker state key a live
// reload added (ADR 0024), so the broker holds nothing the empty managed
// subscription history the reload recorded does not know. A session that
// loaded a non-empty history leaves the broker session alone: another instance
// already connected as the identity and recorded what it subscribed, as after
// a failover. A failure fails Start, which the session manager retries, so the
// session never resumes a broker session its history does not describe. The
// retry reads the history again, because another instance may have filled it
// meanwhile.
func (s *Session) startFreshBrokerSession(ctx context.Context) error {
	s.mu.Lock()
	pending := s.freshBrokerSessionPending
	historyEmpty := len(s.managedHistory) == 0
	if pending && !historyEmpty {
		s.freshBrokerSessionPending = false
	}
	s.mu.Unlock()
	if !pending || !historyEmpty {
		return nil
	}
	if err := s.callEndBrokerSession(ctx); err != nil {
		s.mu.Lock()
		s.managedLoaded = false
		s.mu.Unlock()
		return fmt.Errorf("mqtt: session %q: end the broker session of a newly added client ID before connecting: %w",
			s.metricSessionID(), err)
	}
	s.mu.Lock()
	s.freshBrokerSessionPending = false
	s.mu.Unlock()
	return nil
}

// callEndBrokerSession runs endBrokerSession, or endBrokerSessionOverride when
// a test set one.
func (s *Session) callEndBrokerSession(ctx context.Context) error {
	if s.endBrokerSessionOverride != nil {
		return s.endBrokerSessionOverride(ctx)
	}
	return s.endBrokerSession(ctx)
}

var _ ports.BrokerStateEnder = (*Session)(nil)

package paho

import (
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// What the connection edge tells health: why a CONNECT keeps failing, and
// whether the broker actually resumed the durable session the mode depends on.

// noteConnectFailure latches the mapped cause of a rejected or failed CONNECT
// and counts it under its bounded BridgeError code.
//
// MQTT authenticates only at CONNECT, and autopaho then retries forever on its
// own goroutine. So this callback is the ONLY place the reason a session cannot
// come back is ever visible: without the latch, readiness goes red while
// Health reports no error at all and the operator has nothing to act on. The
// latch is cleared by the connection-up that ends the outage.
//
// The broker URL is deliberately not a metric dimension (it may carry
// credentials); the error code is bounded and is what separates "rotate the
// credential" from "wait for the broker".
//
// ponytail: the latched cause reaches operators through this metric's code
// dimension and the log, not through /deephealth — ports.SessionHealthDetail
// carries no error text today, and adding raw transport error strings to an
// HTTP surface needs a redaction pass first. Add the field when an operator
// actually needs the cause without log access.
func (s *Session) noteConnectFailure(err *shared.BridgeError) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.connectErr = err
	s.mu.Unlock()

	s.metrics.Counter(MetricMQTTConnectFailures, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID},
		shared.Tag{Key: shared.TagKeyCode, Value: string(err.Code)},
	)
	if s.logger != nil {
		s.logger.Warn("mqtt: connect failed; the session stays down until it succeeds",
			"client_id", s.opts.ClientID,
			"code", string(err.Code),
			"error", err,
		)
	}
}

// resumeExpectedLocked reports whether the CONNECT this CONNACK answers asked
// the broker to RESUME a durable session, so Session Present=false means the
// broker discarded state this session was relying on.
//
// Only persistent and exclusive modes ever ask: ephemeral dials
// clean_start=true on every connect by design. Two pieces of evidence make the
// question answerable without false alarms on a cold start, where Session
// Present=false is simply "nothing existed yet":
//
//   - A connection that FOLLOWS one on this session (connEpoch > 0). autopaho
//     sends CleanStart only on a ConnectionManager's initial connection, so
//     every later connection-up asked to resume.
//   - A non-empty managed subscription history. That durable ledger records
//     filters this client id previously established on the broker, so a first
//     connect that finds no session — an exclusive standby taking over after
//     session_expiry_interval elapsed — is a genuine discontinuity.
//
// A persistent session explicitly configured clean_start=true is excluded: each
// ConnectionManager's initial connect legitimately wipes the session, and
// NewSession already warns that the mode's offline retention is void.
//
// Callers must hold s.mu.
func (s *Session) resumeExpectedLocked() bool {
	switch s.mode {
	case connectivity.SessionExclusive:
		// CleanStart is force-overridden to false for exclusive (acl_dial.go),
		// so an exclusive connect always asks to resume.
	case connectivity.SessionPersistent:
		if s.opts.CleanStart {
			return false
		}
	default:
		return false
	}
	return s.connEpoch > 0 || len(s.managedHistory) > 0
}

// durableResumeLostError is the latch surfaced on SessionHealth.LastError after
// a resume-expecting connect found no broker session. It is never returned to a
// route: the session IS usable again once it re-subscribes; what is gone is the
// continuity across the gap.
func durableResumeLostError() error {
	return shared.ErrNotFound.WithMessage(
		"mqtt: broker did not resume the durable session (Session Present=false); the offline " +
			"QoS 1/2 backlog and broker-side subscriptions queued for this client id are lost")
}

// noteDurableResumeLost counts and announces the discontinuity. It runs with
// s.mu released (the latch itself is set under the lock on the connection-up
// path) so the metric and log calls cannot stall autopaho's connection
// goroutine while holding the session mutex.
func (s *Session) noteDurableResumeLost() {
	s.metrics.Counter(MetricMQTTSessionResumeLost, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	if s.logger != nil {
		s.logger.Warn("mqtt: broker did not resume the durable session on a clean_start=false "+
			"connect (Session Present=false); the queued offline QoS 1/2 backlog and the "+
			"broker-side subscriptions for this client id are gone — re-subscribing restores "+
			"delivery for NEW messages only",
			"client_id", s.opts.ClientID,
			"mode", string(s.mode),
		)
	}
}

// clearResumeLost closes the loss window: the reconcile that converged the plan
// has re-established every desired subscription, so delivery is whole again for
// everything published from here on.
func (s *Session) clearResumeLost() {
	s.mu.Lock()
	s.resumeLostErr = nil
	s.mu.Unlock()
}

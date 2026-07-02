package paho

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Reload tears down the current ConnectionManager and re-runs Start
// with the session's current options. It is intended for rotation
// scenarios that cannot be applied to an existing CM — most notably
// TLS material, which requires a new tls.Config baked into a fresh
// autopaho configuration.
//
// Semantics:
//   - Session state is preserved: the plan, router, and event channel
//     survive the teardown. Subscribers stay subscribed (activeSubs
//     is re-materialised by reconcile when the new CM connects).
//   - liveCreds is preserved, so username/password rotation that has
//     already been applied via ApplyCredentials is not lost.
//   - Close-after-Reload semantics match Close: once closed, Reload
//     returns ErrUnavailable.
//
// Why not named Restart: "Restart" would imply a user-initiated
// restart of the session's configured lifecycle, including re-parsing
// options. Reload is narrower — the options are not re-read, only the
// TLS handshake + transport layer are rebuilt.
func (s *Session) Reload(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("mqtt session is closed; Reload is not allowed after Close")
	}
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	s.connected = false
	s.mu.Unlock()

	if cm != nil {
		_ = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}

	return s.Start(ctx)
}

// Close gracefully disconnects the MQTT session. It is safe to call
// Close multiple times.
//
// Ordering invariant (do not reorder):
//  1. Set s.closed = true under mutex — prevents pushEvent from sending.
//  2. Call cm.Disconnect — may trigger OnConnectError, which calls
//     pushEvent, but the s.closed guard returns early (safe re-entrancy).
//     Synchronous Route (see acl_router.go) means any publish the Paho
//     client is still dispatching is processed-then-acked (no loss); if
//     the socket tears down mid-dispatch the message is left un-acked and
//     the broker redelivers it to the next owner — at-least-once either way.
//  3. Await in-flight handlers (bounded by ctx), then close s.events —
//     safe because step 1 guarantees no concurrent sender can reach the
//     channel send.
//
// Note: this adapter does NOT halt consumption before disconnecting. The
// Paho Router seam ACKs after Route returns and cannot NACK, so a "stop
// pulling new work, then drain" gate would have to drop-and-ACK in-flight
// publishes — silently losing them. Graceful lease step-down (quiesce the
// source before disconnect, e.g. via EnableManualAcknowledgment so ACKs
// only follow emit) is a manager-driven concern tracked outside this
// adapter; until then, step-down relies on broker redelivery above.
func (s *Session) Close(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session closing",
			"client_id", s.opts.ClientID)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.connected = false
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	s.mu.Unlock()

	var disconnErr error
	if cm != nil {
		disconnErr = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}

	done := make(chan struct{})
	go func() { s.router.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if s.logger != nil {
			s.logger.Warn("Close: context expired while waiting for in-flight handlers")
		}
	}
	close(s.events)

	if disconnErr != nil {
		return MapError(disconnErr)
	}
	return nil
}

// handleConnectionUp records that the transport connection is
// (re)established and signals SessionConnected. autopaho invokes the
// OnConnectionUp callback (registered in Start) on every (re)connect,
// which calls this method.
//
// Ownership (finding C7): the runtime session manager is the SINGLE
// owner of reconnect reconciliation. It reacts to the SessionConnected
// event by calling Reconcile, whose outcome is authoritative and whose
// failure propagates out of Manager.Run (finding S9). This method must
// therefore NOT reconcile inline; it only resets local subscription
// state and emits the event.
//
// Ordering is load-bearing: activeSubs is reset to empty BEFORE
// SessionConnected is emitted. The reset happens-before the event, which
// happens-before the manager's reconcile reads activeSubs, so on
// reconnect the manager always observes an empty set and issues a full,
// authoritative re-subscribe. The previous ordering emitted the event
// first, letting the manager's reconcile observe stale subscriptions,
// compute an empty delta, and skip the re-subscribe — which, combined
// with an inline reconcile that swallowed its own failure, could leave a
// topic silently unsubscribed with no error surfaced (finding C7).
//
// Lock discipline (finding C7-N4 — analysed, deliberately deferred): this
// callback takes ONLY s.mu for the activeSubs reset and MUST NOT acquire
// reconcileMu. autopaho invokes OnConnectionUp synchronously on its sole
// connection-management goroutine and documents that the callback "must
// not block" (autopaho.ClientConfig.OnConnectionUp, auto.go). reconcileMu
// is held by reconcile() for the entire duration of a network SUBSCRIBE /
// UNSUBSCRIBE round-trip, so blocking on it here would stall that
// goroutine — the owner of reconnect and error handling — on a network
// call, violating the SDK contract and coupling liveness to paho's
// in-flight-request teardown behaviour. The TOCTOU this guard would close
// (a prior-connect reconcile writing stale subscriptions into activeSubs
// after this reset) is a sub-microsecond, ephemeral-only window whose
// worst case is a redundant re-subscribe or a Subscribe error that goes
// terminal — never silent subscription loss (persistent/exclusive
// sessions resume server-side, so a non-empty activeSubs is correct there
// anyway). The deadlock/contract hazard therefore outweighs the benefit;
// do NOT add reconcileMu here without revisiting the autopaho contract.
func (s *Session) handleConnectionUp() {
	s.mu.Lock()
	s.connected = true
	s.activeSubs = make(map[string]byte)
	s.mu.Unlock()

	s.pushEvent(ports.SessionConnected, nil)

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelDebug, "mqtt: connection up",
			"client_id", s.opts.ClientID)
	}
}

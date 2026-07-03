package paho

import (
	"context"
	"time"

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
//  3. Await in-flight handlers (bounded by ctx), then close s.events —
//     safe because step 1 guarantees no concurrent sender can reach the
//     channel send.
//
// Delivery semantics on Close: the adapter uses manual acknowledgment
// (see delivery.go), so a publish whose Delivery has not been settled
// when the socket tears down is simply never acked — the broker
// redelivers it to the next owner of a persistent/exclusive session.
// Close does NOT drain: it disconnects, waits (bounded by ctx) for
// handler goroutines to return, and leaves unsettled deliveries to
// broker redelivery — at-least-once either way, with downstream
// idempotency/dedup absorbing the duplicates.
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

	// Stop the router's grace-sweep worker before awaiting in-flight
	// dispatch handlers. shutdown signals the worker to exit; a best-effort
	// orphan UNSUBSCRIBE already in flight completes on its own (bounded by
	// orphanUnsubscribeTimeout) and is intentionally NOT awaited here, so
	// Close latency stays decoupled from a network round-trip.
	s.router.shutdown()

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
	s.connUpAt = s.clock().Now().UnixNano()
	s.activeSubs = make(map[string]byte)
	s.mu.Unlock()

	// Restart the router's unmatched-publish grace window for this
	// connection: a resumed clean_start=false session begins delivering
	// its queued backlog on CONNACK before the receivers re-register their
	// filters, so unmatched publishes must be buffered (not judged orphan)
	// for one fresh window per connection.
	s.router.beginGrace()

	s.pushEvent(ports.SessionConnected, nil)

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelDebug, "mqtt: connection up",
			"client_id", s.opts.ClientID)
	}
}

// disconnectSessionTakenOver is the MQTT v5 DISCONNECT reason code the
// broker sends to the OLD connection when another client connects with
// the same ClientID (spec §3.14.2.1; packets.DisconnectSessionTakenOver).
const disconnectSessionTakenOver byte = 0x8E

// takeoverStabilityWindow is how long a connection must have been up
// for a subsequent takeover to be considered a NEW incident (legit
// failover) rather than the continuation of a ClientID-collision storm.
const takeoverStabilityWindow = 30 * time.Second

// handleServerDisconnect observes server-initiated DISCONNECT packets
// (autopaho invokes the OnServerDisconnect hook in its own goroutine).
// Session takeover (0x8E) feeds the collision damping in
// noteSessionTakeover; every other reason code is logged — connection-
// down bookkeeping and events are owned by OnConnectionDown /
// OnConnectError, so no state is mutated here to avoid double signals.
func (s *Session) handleServerDisconnect(code byte) {
	if code == disconnectSessionTakenOver {
		s.noteSessionTakeover()
		return
	}
	if berr := MapDisconnectReasonCode(code); berr != nil && s.logger != nil {
		s.logger.Warn("mqtt: server disconnected the session",
			"client_id", s.opts.ClientID,
			"reason_code", code,
			"error", berr,
		)
	}
}

// noteSessionTakeover counts consecutive session-takeover disconnects
// and dampens the resulting reconnect storm. One takeover is normal
// (Exclusive failover: the standby legitimately claims the ClientID),
// so the first occurrence carries no penalty. Repeated takeovers
// without an intervening stable connection (>= takeoverStabilityWindow
// of uptime) mean two live instances share a client_id and are mutually
// kicking each other; each additional occurrence doubles the reconnect
// backoff penalty (1s, 2s, ... capped at 64s — see takeoverPenalty) and
// from the third occurrence an explicit Error log names the
// misconfiguration. MetricMQTTSessionTakeover counts every occurrence.
func (s *Session) noteSessionTakeover() {
	now := s.clock().Now().UnixNano()
	s.mu.Lock()
	if s.connUpAt != 0 && now-s.connUpAt >= int64(takeoverStabilityWindow) {
		// The connection had been stable: treat this as a fresh
		// incident, not a continuation of a collision storm.
		s.takeoverStreak = 0
	}
	s.takeoverStreak++
	streak := s.takeoverStreak
	s.mu.Unlock()

	s.metrics.Counter(MetricMQTTSessionTakeover, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})

	berr := MapDisconnectReasonCode(disconnectSessionTakenOver).
		With("takeover_count", streak)
	s.pushEvent(ports.SessionReconnecting, berr)

	if s.logger != nil {
		if streak >= 3 {
			s.logger.Error("mqtt: repeated session takeover — another live instance is using the same client_id; "+
				"reconnects are being backed off (fix the client_id collision or the instances will keep kicking each other)",
				"client_id", s.opts.ClientID,
				"takeover_count", streak,
				"backoff_penalty", s.takeoverPenalty(),
			)
		} else {
			s.logger.Warn("mqtt: session taken over by another connection with the same client_id",
				"client_id", s.opts.ClientID,
				"takeover_count", streak,
			)
		}
	}
}

// takeoverPenalty returns the extra reconnect delay applied on top of
// the configured backoff while a takeover storm is in progress:
// 0 for streak <= 1 (single takeover = legitimate failover; the standby
// must not be slowed down), then 1s << (streak-2) capped at 64s.
func (s *Session) takeoverPenalty() time.Duration {
	s.mu.Lock()
	streak := s.takeoverStreak
	s.mu.Unlock()
	if streak <= 1 {
		return 0
	}
	shift := streak - 2
	if shift > 6 {
		shift = 6
	}
	return time.Duration(1<<shift) * time.Second
}

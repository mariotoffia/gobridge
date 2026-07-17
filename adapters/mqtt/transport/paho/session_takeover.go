package paho

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

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
//
// Exception (HIGH-3): when shared subscriptions ($share) are active and the
// mode is NOT Exclusive, a single takeover already proves the scale-out
// client_id-collision self-DOS (replicas that must be unique are sharing an
// identity), so the Error log fires on the FIRST occurrence and names the
// unique-client_id requirement. The backoff penalty is unchanged (still
// streak-driven) — only the log severity/message is escalated.
func (s *Session) noteSessionTakeover() {
	now := s.clock().Now().UnixNano()
	s.mu.Lock()
	if s.connUpAt != 0 && now-s.connUpAt >= int64(takeoverStabilityWindow) {
		// The connection had been stable: treat this as a fresh
		// incident, not a continuation of a collision storm.
		s.takeoverStreak = 0
	}
	s.takeoverStreak++
	s.lastTakeoverAt = now
	streak := s.takeoverStreak
	// A takeover while shared subscriptions ($share) are active is the
	// smoking gun of the HIGH-3 self-DOS: another instance connected with the
	// SAME client_id where scale-out demands UNIQUE ones. The only mode where
	// a shared client_id is legitimate is Exclusive (a single leaseholder, so
	// a takeover there is a normal lease handoff, not a collision). For every
	// other mode, escalate on the FIRST occurrence rather than waiting for the
	// streak to prove a storm.
	sharedSubsActive := s.planHasSharedSubscriptionsLocked()
	s.mu.Unlock()

	sharedSubCollision := sharedSubsActive && s.mode != connectivity.SessionExclusive

	s.metrics.Counter(MetricMQTTSessionTakeover, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})

	berr := MapDisconnectReasonCode(disconnectSessionTakenOver).
		With("takeover_count", streak)
	s.pushEvent(ports.SessionReconnecting, berr)

	if s.logger != nil {
		switch {
		case sharedSubCollision:
			s.logger.Error("mqtt: session takeover while shared subscriptions ($share) are active — another "+
				"instance is using the SAME client_id where scale-out REQUIRES a UNIQUE client_id per instance; "+
				"the instances are taking each other over (self-DOS) and load-balancing is broken. Give each "+
				"replica a unique client_id, or move to an exclusive lease if a single active owner is intended",
				"client_id", s.opts.ClientID,
				"session_mode", s.mode,
				"takeover_count", streak,
				"backoff_penalty", s.takeoverPenalty(),
			)
		case streak >= 3:
			s.logger.Error("mqtt: repeated session takeover — another live instance is using the same client_id; "+
				"reconnects are being backed off (fix the client_id collision or the instances will keep kicking each other)",
				"client_id", s.opts.ClientID,
				"takeover_count", streak,
				"backoff_penalty", s.takeoverPenalty(),
			)
		default:
			s.logger.Warn("mqtt: session taken over by another connection with the same client_id",
				"client_id", s.opts.ClientID,
				"takeover_count", streak,
			)
		}
	}
}

// takeoverPenalty returns the extra reconnect delay applied on top of
// the configured backoff while a takeover storm is IN PROGRESS:
// 0 for streak <= 1 (single takeover = legitimate failover; the standby
// must not be slowed down), then 1s << (streak-2) capped at 64s.
//
// The penalty is gated on recency (A-4): it only applies while takeovers are
// still actively arriving (the last one within takeoverStabilityWindow). Once
// the collision resolves and no takeover has occurred for that window, the
// penalty decays to 0 even though takeoverStreak is still high — an ordinary
// reconnect (a network blip long after the storm) must not be stuck paying a
// stale storm's backoff. The streak itself is only reset when a NEW takeover
// arrives after a stable connection (noteSessionTakeover), so a takeover
// recurring within the window resumes damping at the accumulated level.
func (s *Session) takeoverPenalty() time.Duration {
	now := s.clock().Now().UnixNano()
	s.mu.Lock()
	streak := s.takeoverStreak
	last := s.lastTakeoverAt
	s.mu.Unlock()
	if streak <= 1 || last == 0 || now-last >= int64(takeoverStabilityWindow) {
		return 0
	}
	shift := streak - 2
	if shift > 6 {
		shift = 6
	}
	return time.Duration(1<<shift) * time.Second
}

package paho

import "time"

// connectionStabilityWindow is how long a connection must stay up before a
// fault on it counts as a new incident rather than the continuation of a
// storm. Session-takeover damping and pre-decode ingress-reject damping both
// use it.
const connectionStabilityWindow = 30 * time.Second

// reconnectBackoff computes the delay before autopaho's Nth (re)connect
// attempt. autopaho calls this with attempt 0 (the delay BEFORE the first
// attempt — always 0), then 1, 2, ... after each failure. The base delay
// grows exponentially from `base` by `factor` per attempt, capped at
// `maxDelay`, then EQUAL-JITTER spreads it over [d/2, d).
//
// Why jitter: a fleet of bridge instances that all lose the same
// broker will otherwise retry on identical wall-clock boundaries, hammering
// the broker in synchronised waves as it comes back (thundering herd).
// Equal-jitter (matching runtime/bridge.go's equalJitter and
// runtime/route/retry.go's applyJitter conventions) desynchronises them
// while keeping a meaningful minimum spacing.
//
// randFloat supplies a value in [0,1); production passes
// math/rand/v2.Float64, tests inject a deterministic source (no sleeps).
func reconnectBackoff(attempt int, base, maxDelay time.Duration, factor float64, randFloat func() float64) time.Duration {
	if attempt <= 0 {
		return 0
	}
	if base <= 0 {
		base = DefaultReconnectDelay
	}
	if maxDelay < base {
		maxDelay = base
	}
	if factor <= 1 {
		factor = reconnectBackoffFactor
	}
	// Capped exponential base delay: attempt 1 -> base, 2 -> base*factor, ...
	d := float64(base)
	for i := 1; i < attempt; i++ {
		d *= factor
		if d >= float64(maxDelay) {
			d = float64(maxDelay)
			break
		}
	}
	// Equal-jitter: wait ∈ [d/2, d).
	half := d / 2
	return time.Duration(half + randFloat()*half)
}

// reconnectBackoffConfig resolves the reconnect-backoff envelope from the
// session options, applying defaults and clamping maxDelay >= base so a
// misconfigured reconnect_max_delay < reconnect_delay cannot invert the
// envelope.
func (s *Session) reconnectBackoffConfig() (base, maxDelay time.Duration) {
	base = s.opts.ReconnectDelay
	if base <= 0 {
		base = DefaultReconnectDelay
	}
	maxDelay = s.opts.ReconnectMaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultReconnectMaxDelay
	}
	if maxDelay < base {
		maxDelay = base
	}
	return base, maxDelay
}

// newReconnectBackoff builds the autopaho ReconnectBackoff function: a
// jittered exponential base delay (reconnectBackoff) PLUS the escalating
// session-takeover penalty (noteSessionTakeover) PLUS the pre-decode
// ingress-reject penalty (ingressRejectPenalty), so a ClientID collision or a
// broker that keeps sending a packet the guard refuses backs off on top of the
// normal envelope. Both penalties apply at every attempt, including attempt 0
// — the first redial after a connection that came up and the first dial of a
// fresh connection manager — where the base delay is 0. randFloat is
// injectable for tests; production passes math/rand/v2.Float64.
func (s *Session) newReconnectBackoff(randFloat func() float64) func(int) time.Duration {
	base, maxDelay := s.reconnectBackoffConfig()
	return func(attempt int) time.Duration {
		return reconnectBackoff(attempt, base, maxDelay, reconnectBackoffFactor, randFloat) +
			s.takeoverPenalty() +
			s.ingressRejectPenalty(base, maxDelay, randFloat)
	}
}

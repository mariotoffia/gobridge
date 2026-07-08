package paho

import "time"

// reconnectBackoff computes the delay before autopaho's Nth (re)connect
// attempt. autopaho calls this with attempt 0 (the delay BEFORE the first
// attempt — always 0), then 1, 2, ... after each failure. The base delay
// grows exponentially from `base` by `factor` per attempt, capped at
// `maxDelay`, then EQUAL-JITTER spreads it over [d/2, d).
//
// Why jitter (M-4): a fleet of bridge instances that all lose the same
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
// session-takeover penalty (noteSessionTakeover), so a ClientID collision
// backs off on top of the normal envelope. randFloat is injectable for
// tests; production passes math/rand/v2.Float64.
func (s *Session) newReconnectBackoff(randFloat func() float64) func(int) time.Duration {
	base, maxDelay := s.reconnectBackoffConfig()
	return func(attempt int) time.Duration {
		return reconnectBackoff(attempt, base, maxDelay, reconnectBackoffFactor, randFloat) + s.takeoverPenalty()
	}
}

package route

import (
	"math/rand/v2"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// RetryDelay returns the next retry delay for a recoverable send
// error: the broker-provided RetryAfter when present (returned as-is,
// never jittered), otherwise an exponentially-backed-off interval
// derived from policy.Backoff, capped by Backoff.MaxInterval and
// optionally randomized by Backoff.JitterFactor (equal-jitter).
func RetryDelay(policy routing.RoutePolicy, attempt int, sendErr error) time.Duration {
	return retryDelay(policy, attempt, sendErr, rand.Float64)
}

// retryDelay is the testable core of RetryDelay. randFloat supplies a
// value in [0,1) used for equal-jitter; production passes
// math/rand/v2.Float64, tests inject a deterministic source.
func retryDelay(policy routing.RoutePolicy, attempt int, sendErr error, randFloat func() float64) time.Duration {
	if d := shared.GetRetryAfter(sendErr); d > 0 {
		// Broker-provided hint is authoritative; do not jitter it.
		return d
	}
	bp := policy.Backoff
	if bp.InitialInterval <= 0 || bp.Multiplier <= 0 {
		bp = routing.NewDefaultBackoffPolicy()
	}
	if attempt < 1 {
		attempt = 1
	}
	// Capped exponential backoff.
	d := float64(bp.InitialInterval)
	for i := 1; i < attempt; i++ {
		d *= bp.Multiplier
		if bp.MaxInterval > 0 && d >= float64(bp.MaxInterval) {
			d = float64(bp.MaxInterval)
			break
		}
	}
	return applyJitter(d, bp, randFloat)
}

// applyJitter applies equal-jitter to the computed delay d (nanoseconds
// as float64): delay = d*(1-jf) + rand[0, d*jf), clamped to
// [0, MaxInterval]. JitterFactor <= 0 returns d unchanged so callers
// that opt out keep the deterministic exponential delay.
func applyJitter(d float64, bp routing.BackoffPolicy, randFloat func() float64) time.Duration {
	jf := bp.JitterFactor
	if jf <= 0 {
		return time.Duration(d)
	}
	if jf > 1 {
		jf = 1
	}
	delay := d*(1-jf) + randFloat()*(d*jf)
	if bp.MaxInterval > 0 && delay > float64(bp.MaxInterval) {
		delay = float64(bp.MaxInterval)
	}
	if delay < 0 {
		delay = 0
	}
	return time.Duration(delay)
}

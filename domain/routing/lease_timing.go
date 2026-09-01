package routing

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Lease cadence baselines. They live here, in the domain, rather than in the
// session manager because THREE boundaries must agree on them: the blueprint
// validator (which must reject a bad cadence before a durable config write), the
// builder (which maps a route's session block onto them), and the manager
// itself. A boundary that re-derives its own subset always drifts from the one
// that runs, and a validator that judges a configuration by values the manager
// will not use is worse than no validator at all.
const (
	// DefaultLeaseTTL is the baseline lease lifetime for a non-clustered
	// deployment.
	DefaultLeaseTTL = 360 * time.Second
	// DefaultRenewInterval is pinned rather than derived so the final renew
	// attempt lands off the expiry boundary: 110*3 = 330s < 360s.
	DefaultRenewInterval = 110 * time.Second
	// DefaultRenewJitter spreads renewals across a fleet on the default preset.
	DefaultRenewJitter = 5 * time.Second
	// DefaultMaxRenewFails is the consecutive-renewal-failure tolerance before
	// step-down.
	DefaultMaxRenewFails = 3

	// The high-availability baseline. Its worst-case renew span is
	// 3 x (10 + 0.5 + 3) = 40.5s, strictly under the 45s TTL.
	HALeaseTTL         = 45 * time.Second
	HARenewInterval    = 10 * time.Second
	HARenewJitter      = 1 * time.Second
	HARenewCallTimeout = 3 * time.Second
	HAStepDownGrace    = 5 * time.Second

	// MinimumLeaseCadence is the floor for the RESOLVED renew interval and
	// standby acquire poll. Below it the lease store, not the timing model,
	// decides ownership: the owner issues renew writes back to back and every
	// standby issues a full claim round per tick, the backend throttles, and the
	// throttling errors feed the very transient-failure counter that triggers
	// step-down. 250ms is generous for any supported store (a DynamoDB
	// UpdateItem/PutItem round trip is a few milliseconds) while still an order
	// of magnitude below the fastest cadence any supported baseline derives, so
	// it rejects only genuinely collapsed configurations.
	MinimumLeaseCadence = 250 * time.Millisecond
)

// LeaseTimingRequest is what an operator asked for. A zero duration means
// "unset, derive it"; a zero MaxRenewFails means "unset, use the default".
type LeaseTimingRequest struct {
	LeaseTTL            time.Duration
	RenewInterval       time.Duration
	RenewJitter         time.Duration
	RenewCallTimeout    time.Duration
	AcquirePollInterval time.Duration
	MaxRenewFails       int
}

// LeaseTiming is what will actually run: the request after defaults, derivation
// and the expiry-margin clamp.
type LeaseTiming struct {
	LeaseTTL            time.Duration
	RenewInterval       time.Duration
	RenewJitter         time.Duration
	RenewCallTimeout    time.Duration
	AcquirePollInterval time.Duration
	// MaxRenewFails is the RESOLVED tolerance, so a diagnostic never prints the
	// raw zero an operator did not write.
	MaxRenewFails int
	// RequestedRenewInterval and RequestedRenewCallTimeout are the values that
	// went INTO the clamp, so a diagnostic can name what was asked for next to
	// what would run.
	RequestedRenewInterval    time.Duration
	RequestedRenewCallTimeout time.Duration
	// CadenceClamped reports that the clamp had to shrink the renew interval or
	// the per-call timeout — not merely shed jitter. The distinction is the whole
	// point: ClampRenewTimings sheds jitter FIRST because jitter only spreads
	// load, and a cadence that fits once jitter is trimmed is perfectly healthy.
	// Having to cut the interval or the call timeout instead means the requested
	// failure tolerance does not fit the TTL at all, and what would run is not
	// what was written.
	CadenceClamped bool
}

// BaselineLeaseTiming returns the cadence a route session inherits before its
// own overrides. A clustered deployment that pinned NEITHER lease_ttl NOR
// renew_interval takes the lower-latency HA baseline; pinning either means the
// operator is tuning the cadence, so the default baseline is kept and only the
// TTL survives (interval and jitter fall back to derivation, which follows the
// TTL instead of contradicting it).
func BaselineLeaseTiming(clustered bool, req LeaseTimingRequest) LeaseTimingRequest {
	if clustered && req.LeaseTTL == 0 && req.RenewInterval == 0 {
		return LeaseTimingRequest{
			LeaseTTL:         HALeaseTTL,
			RenewInterval:    HARenewInterval,
			RenewJitter:      HARenewJitter,
			RenewCallTimeout: HARenewCallTimeout,
			MaxRenewFails:    DefaultMaxRenewFails,
		}
	}
	return LeaseTimingRequest{LeaseTTL: DefaultLeaseTTL, MaxRenewFails: DefaultMaxRenewFails}
}

// ApplyOverrides layers an operator's explicitly pinned values onto a baseline.
// Only positive values override, mirroring "empty means inherit" in the
// blueprint.
func (r LeaseTimingRequest) ApplyOverrides(over LeaseTimingRequest) LeaseTimingRequest {
	if over.LeaseTTL > 0 {
		r.LeaseTTL = over.LeaseTTL
	}
	if over.RenewInterval > 0 {
		r.RenewInterval = over.RenewInterval
	}
	if over.RenewJitter > 0 {
		r.RenewJitter = over.RenewJitter
	}
	if over.RenewCallTimeout > 0 {
		r.RenewCallTimeout = over.RenewCallTimeout
	}
	if over.AcquirePollInterval > 0 {
		r.AcquirePollInterval = over.AcquirePollInterval
	}
	if over.MaxRenewFails > 0 {
		r.MaxRenewFails = over.MaxRenewFails
	}
	return r
}

// Resolve applies defaults, derivation and the expiry-margin clamp in the exact
// order the session manager applies them: TTL, then MaxRenewFails, then the
// derived-or-pinned renew interval, then jitter (derived ONLY when the interval
// was also unset — a pinned interval is explicit enough that a zero jitter means
// "no jitter"), then the per-call timeout, then the clamp, and finally the
// standby acquire poll derived from the CLAMPED interval.
func (r LeaseTimingRequest) Resolve() LeaseTiming {
	t := LeaseTiming{LeaseTTL: r.LeaseTTL}
	if t.LeaseTTL <= 0 {
		t.LeaseTTL = DefaultLeaseTTL
	}
	t.MaxRenewFails = r.MaxRenewFails
	if t.MaxRenewFails <= 0 {
		t.MaxRenewFails = DefaultMaxRenewFails
	}

	t.RenewInterval = r.RenewInterval
	renewDerived := t.RenewInterval <= 0
	if renewDerived {
		t.RenewInterval = DeriveRenewInterval(t.LeaseTTL, t.MaxRenewFails)
	}
	t.RenewJitter = r.RenewJitter
	if t.RenewJitter < 0 {
		t.RenewJitter = 0
	}
	if t.RenewJitter == 0 && renewDerived {
		t.RenewJitter = DeriveRenewJitter(t.RenewInterval)
	}
	t.RenewCallTimeout = r.RenewCallTimeout
	if t.RenewCallTimeout <= 0 {
		t.RenewCallTimeout = DeriveRenewCallTimeout(t.RenewInterval)
	}

	t.RequestedRenewInterval = t.RenewInterval
	t.RequestedRenewCallTimeout = t.RenewCallTimeout
	t.RenewInterval, t.RenewJitter, t.RenewCallTimeout, _ = ClampRenewTimings(
		t.LeaseTTL, t.RenewInterval, t.RenewJitter, t.RenewCallTimeout, t.MaxRenewFails,
	)
	t.CadenceClamped = t.RenewInterval != t.RequestedRenewInterval ||
		t.RenewCallTimeout != t.RequestedRenewCallTimeout

	t.AcquirePollInterval = r.AcquirePollInterval
	if t.AcquirePollInterval <= 0 {
		t.AcquirePollInterval = DeriveAcquirePollInterval(t.RenewInterval, t.LeaseTTL)
	}
	return t
}

// ValidateCadence rejects a resolved cadence that the manager would only reach
// by clamping, or one no lease store could serve. `subject` names the session in
// the diagnostic.
func (t LeaseTiming) ValidateCadence(subject string) error {
	if t.CadenceClamped {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"%s: LeaseTTL=%s with MaxRenewFails=%d leaves no room for the renew cadence: "+
				"RenewInterval=%s and RenewCallTimeout=%s fit under the TTL only after being cut to %s and %s. "+
				"Lower MaxRenewFails, raise LeaseTTL, or pin a shorter renew_interval",
			subject, t.LeaseTTL, t.MaxRenewFails,
			t.RequestedRenewInterval, t.RequestedRenewCallTimeout, t.RenewInterval, t.RenewCallTimeout,
		))
	}
	if t.RenewInterval < MinimumLeaseCadence {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"%s: resolved RenewInterval=%s is below the minimum lease cadence=%s; "+
				"the owner would renew faster than the lease store can serve",
			subject, t.RenewInterval, MinimumLeaseCadence,
		))
	}
	if t.AcquirePollInterval < MinimumLeaseCadence {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"%s: resolved AcquirePollInterval=%s is below the minimum lease cadence=%s; "+
				"every standby would claim faster than the lease store can serve",
			subject, t.AcquirePollInterval, MinimumLeaseCadence,
		))
	}
	return nil
}

// DeriveRenewInterval returns a renew interval that places the FULL
// MaxRenewFails-th worst-case detection span — interval + jitter/2 + per-call
// renew timeout — at ~75% of the TTL, reserving ~25% of the TTL as
// headroom for clock slack before the lease would expire. This is the
// production derivation path once an operator supplies only LeaseTTL: the
// owner tolerates MaxRenewFails-1 transient renew failures and the recovering
// attempt still lands before the expiry boundary, even when every failing renew
// call burns the full RenewCallTimeout before failing.
func DeriveRenewInterval(ttl time.Duration, maxRenewFails int) time.Duration {
	if maxRenewFails < 1 {
		maxRenewFails = 1
	}
	// Budget per attempt for interval + jitter/2 + callTimeout, targeting 75%
	// of the TTL across all MaxRenewFails attempts.
	perAttempt := (ttl * 3) / (time.Duration(maxRenewFails) * 4)
	// Reserve the per-call renew timeout (bounded by its 5s cap) so it does not
	// erode the headroom the way it once did. Never let the reserve
	// consume more than half the per-attempt budget for tiny TTLs.
	reserve := 5 * time.Second
	if reserve > perAttempt/2 {
		reserve = perAttempt / 2
	}
	// jitter is interval/4 (DeriveRenewJitter), so jitter/2 = interval/8. Solve
	// interval + interval/8 + reserve = perAttempt  ⇒  interval = (perAttempt-reserve)*8/9.
	interval := ((perAttempt - reserve) * 8) / 9
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	return interval
}

// DeriveRenewJitter returns a jitter derived from the renew interval rather
// than a fixed constant: a quarter of the interval spreads renewals
// enough to avoid a thundering herd while contributing only RenewInterval/8
// per attempt to the worst-case span, comfortably inside the headroom
// reserved by DeriveRenewInterval.
func DeriveRenewJitter(renewInterval time.Duration) time.Duration {
	j := renewInterval / 4
	if j < 0 {
		j = 0
	}
	return j
}

// DeriveAcquirePollInterval returns how often a standby retries Acquire while
// waiting to take over. A standby must poll FASTER than the owner renews so
// failover is bounded by ~LeaseTTL rather than by the owner's renew cadence:
// the smaller of the renew interval and a quarter of the TTL, capped at 5s so
// even large-TTL deployments retry promptly, and floored to avoid a busy loop.
func DeriveAcquirePollInterval(renewInterval, ttl time.Duration) time.Duration {
	poll := renewInterval
	if q := ttl / 4; q > 0 && q < poll {
		poll = q
	}
	const maxPoll = 5 * time.Second
	if poll > maxPoll {
		poll = maxPoll
	}
	if poll < time.Millisecond {
		poll = time.Millisecond
	}
	return poll
}

// DeriveRenewCallTimeout bounds a single Acquire/Renew store call at
// min(RenewInterval/2, 5s), floored at 1s so tiny (test) intervals do not
// create spuriously short deadlines. This stops a hung DynamoDB call from
// stretching step-down and takeover unboundedly.
func DeriveRenewCallTimeout(renewInterval time.Duration) time.Duration {
	const (
		maxTimeout = 5 * time.Second
		minTimeout = 1 * time.Second
	)
	t := renewInterval / 2
	if t > maxTimeout {
		t = maxTimeout
	}
	if t < minTimeout {
		t = minTimeout
	}
	return t
}

// RenewWorstCaseSpan is the maximum wall-clock time the owner may take to
// detect a definitive lease loss through renewal failures: MaxRenewFails
// attempts, each delayed by the renew interval plus the maximum positive
// jitter (RenewJitter/2) AND the time a single renew call may burn before it
// fails (up to RenewCallTimeout).
//
// The session manager's renew loop resets the renew timer AFTER the renew call
// returns, so the real spacing between consecutive attempts is
// interval + jitter/2 + call-duration, and a hung backend burns the full
// RenewCallTimeout on every attempt. Omitting RenewCallTimeout under-counts the
// span by MaxRenewFails × RenewCallTimeout, which in short-TTL profiles can
// push real detection PAST the lease TTL. Keeping this
// strictly below LeaseTTL guarantees the owner detects loss and steps down
// before its OWN lease would expire, so it stops sending before a new owner
// takes over.
func RenewWorstCaseSpan(renewInterval, renewJitter, renewCallTimeout time.Duration, maxRenewFails int) time.Duration {
	if maxRenewFails < 1 {
		maxRenewFails = 1
	}
	if renewCallTimeout < 0 {
		renewCallTimeout = 0
	}
	perAttempt := renewInterval + renewJitter/2 + renewCallTimeout
	return perAttempt * time.Duration(maxRenewFails)
}

// ClampRenewTimings enforces the expiry-margin invariant defensively at
// construction time: if the worst-case renew span (which now folds in the
// per-call renew timeout) meets or exceeds the TTL it sheds jitter
// first (cheap — jitter only spreads load), then, if still unsafe, reserves the
// per-call timeout budget and shrinks the renew interval so the span fits within
// 90% of the TTL. In the pathological case where the per-call timeout alone
// exhausts a per-attempt budget it is clamped too. It returns the (possibly
// adjusted) values and whether any clamp occurred so the caller can warn. This
// is a safety net; LeaseTiming.ValidateCadence reports a clamp that had to cut the
// interval or the per-call timeout as a hard error.
func ClampRenewTimings(ttl, renewInterval, renewJitter, renewCallTimeout time.Duration, maxRenewFails int) (time.Duration, time.Duration, time.Duration, bool) {
	if maxRenewFails < 1 {
		maxRenewFails = 1
	}
	if renewInterval < time.Millisecond {
		renewInterval = time.Millisecond
	}
	if renewJitter < 0 {
		renewJitter = 0
	}
	if renewCallTimeout < 0 {
		renewCallTimeout = 0
	}
	if RenewWorstCaseSpan(renewInterval, renewJitter, renewCallTimeout, maxRenewFails) < ttl {
		return renewInterval, renewJitter, renewCallTimeout, false
	}

	limit := (ttl * 9) / 10 // hard ceiling for the worst-case span
	perAttemptLimit := limit / time.Duration(maxRenewFails)

	// The per-call timeout consumes part of every attempt's budget. If it does
	// not even leave room for a minimal renew interval, clamp it too — a
	// hung-call ceiling larger than the whole per-attempt budget can never
	// satisfy the invariant on its own.
	if renewCallTimeout > perAttemptLimit-time.Millisecond {
		renewCallTimeout = perAttemptLimit - time.Millisecond
		if renewCallTimeout < 0 {
			renewCallTimeout = 0
		}
	}
	available := perAttemptLimit - renewCallTimeout // budget for interval + jitter/2

	if renewInterval < available {
		if maxJitter := 2 * (available - renewInterval); renewJitter > maxJitter {
			renewJitter = maxJitter
		}
		if RenewWorstCaseSpan(renewInterval, renewJitter, renewCallTimeout, maxRenewFails) < ttl {
			return renewInterval, renewJitter, renewCallTimeout, true
		}
	}

	// Jitter alone insufficient (renew interval already too large): drop jitter
	// and shrink the interval to fit the budget left after the per-call timeout.
	renewJitter = 0
	renewInterval = available
	if renewInterval < time.Millisecond {
		renewInterval = time.Millisecond
	}
	return renewInterval, renewJitter, renewCallTimeout, true
}

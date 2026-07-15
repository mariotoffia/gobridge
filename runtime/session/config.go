package session

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// MinimumProductionLeaseTTL is the uniform lower bound accepted by production
// composition roots and Config.Validate. Direct manager construction remains
// available to deterministic tests that intentionally use compressed time.
const MinimumProductionLeaseTTL = 5 * time.Second

// MaxStartupAllowance bounds the explicit process-start contribution to a
// declared failover SLO. Larger values indicate an unbounded deployment path.
const MaxStartupAllowance = 10 * time.Minute

// Config configures session management for exclusive sessions.
type Config struct {
	SessionID string
	Exclusive bool
	Plan      connectivity.SessionPlan

	// LeaseTTL is how long a lease is valid before it expires in the
	// backing store. Longer values tolerate network interruptions but
	// increase failover time. Default: 360s.
	LeaseTTL time.Duration

	// RenewInterval is how often the lease is renewed. Zero means
	// "derive from LeaseTTL / MaxRenewFails with expiry margin" at
	// construction time (see deriveRenewInterval).
	RenewInterval time.Duration

	// RenewJitter adds random jitter to each renewal timer to avoid
	// thundering-herd effects. Zero means "derive from RenewInterval"
	// at construction time (see deriveRenewJitter); the jitter is
	// included in the expiry-margin invariant so it can never push the
	// worst-case renew span past the TTL.
	RenewJitter time.Duration

	// MaxRenewFails is the consecutive renewal failures tolerated
	// before the session manager initiates step-down. Default: 3.
	// Definitive lease-loss signals (stale fencing token, not-found)
	// bypass this counter and step down immediately.
	MaxRenewFails int

	// AcquirePollInterval is how often a standby retries Acquire while
	// waiting to take over a lease. It is deliberately DECOUPLED from
	// RenewInterval: a standby must poll FASTER than the owner renews so
	// failover is bounded by ~LeaseTTL rather than by the owner's renew
	// cadence. Zero means "derive" (see deriveAcquirePollInterval).
	AcquirePollInterval time.Duration

	// RenewCallTimeout bounds a single lease-store Acquire/Renew call so a
	// hung backend cannot stretch step-down and takeover unboundedly. Zero
	// means "derive as min(RenewInterval/2, 5s)" (see
	// deriveRenewCallTimeout).
	RenewCallTimeout time.Duration

	// StepDownGrace is how long the session waits for in-flight
	// outbox Send+Complete operations to finish before releasing
	// the lease. This is I/O-bound, not lease-bound. Default: 15s.
	StepDownGrace time.Duration

	DrainStrategy       persistence.DrainStrategy
	DrainBatchSize      int
	DrainMaxBatchSize   int
	DrainMaxConcurrency int

	// DrainTimeout is the legacy fixed ceiling applied to a single
	// drain batch when both PerRecordDrainTimeout and MaxDrainTimeout
	// are zero. Retained for backward compatibility.
	DrainTimeout time.Duration
	// PerRecordDrainTimeout feeds the scaled formula for the batch
	// ceiling: ceiling = min(batchCount * PerRecordDrainTimeout,
	// MaxDrainTimeout). Setting either this or MaxDrainTimeout
	// activates the scaled formula and supersedes DrainTimeout.
	PerRecordDrainTimeout time.Duration
	// MaxDrainTimeout is the upper bound of the scaled drain formula.
	MaxDrainTimeout time.Duration

	// PostAcquireActivationTimeout is the conservative hard bound for the
	// complete initial Start/Reconcile/migration sequence after lease acquisition.
	// Composition roots derive it from the stateful transport's typed timing
	// capability. Zero preserves the direct-manager fallback to the remaining
	// locally safe lease window.
	PostAcquireActivationTimeout time.Duration

	// FailoverSLO is the optional failure-detection to ServiceLevelFull objective.
	// Zero means no objective is declared.
	FailoverSLO time.Duration

	// StartupAllowance reserves explicit bounded process startup work not already
	// represented by lease, broker-connect, or reconcile durations.
	StartupAllowance time.Duration

	// ConnectAfterLease defers session.Start until the lease is acquired.
	// This avoids connecting to a broker (e.g. MQTT with an exclusive
	// ClientID) before ownership is confirmed, which would disconnect
	// the current owner prematurely.
	ConnectAfterLease bool
}

// DefaultConfig returns a Config with recommended defaults.
//
// RenewInterval is pinned to 110s rather than left zero (which would derive
// LeaseTTL/MaxRenewFails = 120s and place the third renew attempt exactly on
// the 360s expiry boundary). At 110s the three attempts sum to 330s, strictly
// under LeaseTTL, so the owner tolerates two transient renew failures and the
// third (recovering) attempt lands ~30s before expiry instead of racing it
// (A8-R1-leasettl-margin). A custom Config that leaves RenewInterval zero
// still derives LeaseTTL/MaxRenewFails at construction time.
func DefaultConfig(sessionID string, exclusive bool) Config {
	return Config{
		SessionID:           sessionID,
		Exclusive:           exclusive,
		LeaseTTL:            360 * time.Second,
		RenewInterval:       110 * time.Second, // 110*3=330s < 360s TTL: final renew off the expiry boundary (A8-R1)
		RenewJitter:         5 * time.Second,
		MaxRenewFails:       3,
		StepDownGrace:       routing.DefaultStepDownGrace,
		DrainStrategy:       persistence.NewFixedPoll(persistence.DefaultFixedPollInterval),
		DrainBatchSize:      100,
		DrainMaxBatchSize:   500,
		DrainMaxConcurrency: 10,
	}
}

// HAConfig returns a Config with a lower-latency lease renewal cadence for
// high-availability clusters. It starts from DefaultConfig and tightens only
// lease-timing knobs. It is not an end-to-end failover SLO: broker activation,
// reconciliation, startup, and acquisition polling require separate validation.
//
// Timing: LeaseTTL=45s, RenewInterval=10s, MaxRenewFails=3, RenewJitter=1s,
// RenewCallTimeout=3s, StepDownGrace=5s. Those values encode these invariants:
//
//   - Takeover requires a full TTL of persisted unchanged-tuple evidence. A
//     replacement observer inherits already-confirmed evidence from the lease row.
//   - StepDownGrace < LeaseTTL (5s vs 45s): the only step-down trigger is
//     involuntary — MaxRenewFails consecutive renew failures, detected at
//     roughly LeaseTTL — after which the owner stops claiming and drains
//     in-flight Send+Complete for StepDownGrace before releasing. Keeping it
//     well under LeaseTTL bounds that drain. It does NOT by itself order the
//     old owner's last send ahead of the new owner's first: single-owner
//     safety comes from the lease store (one non-expired lease at a time) and
//     from version fencing on outbox Complete and Claim, independent of these
//     timings. A brief duplicate *send* — never a duplicate commit — is
//     possible during the failover overlap (the same at-least-once window as
//     DefaultConfig), so downstream consumers must be idempotent.
//   - Worst-case loss detection folds in the per-call renew timeout because
//     renewLoop resets its timer AFTER each renew call returns (finding H2):
//     MaxRenewFails × (RenewInterval + RenewJitter/2 + RenewCallTimeout) =
//     3 × (10 + 0.5 + 3) = 40.5s < 45s TTL, so the owner detects a definitive
//     lease loss and steps down with a ~4.5s (10%) margin before its own lease
//     would expire — even when every failing renew call burns the full
//     RenewCallTimeout. (The earlier 14s/2s/no-call-timeout preset summed to
//     58.5s > 45s, i.e. 13.5s PAST expiry; A9-J5.)
//   - RenewJitter (1s) stays small relative to the 10s RenewInterval so late
//     renewals cannot drift the worst-case span past the TTL.
//
// Tradeoff: faster failover costs more lease-store renewal writes (every ~10s
// vs ~110s) and tolerates fewer transient renew failures, so blip-prone
// networks see more spurious step-downs. For such networks relax LeaseTTL
// toward 60s.
//
// Cross-config note (shared DynamoDB outbox only): keep stale_claim_duration
// (~20s) above the worst-case drain-batch timeout so a batch still in flight
// is not reclaimed mid-send. It does NOT gate failover hand-off — a new owner
// reclaims the prior owner's records at once via its strictly higher fencing
// version (memory/SQLite reclaim is version-only; DynamoDB adds the time-stale
// path only for same-version stranded claims, having no OutboxReleaser fast
// path). StepDownGrace + 15s (~20s) is just a convenient value that clears
// that drain ceiling.
//
// The 45s LeaseTTL trades renewal-write rate against lease-loss detection.
// Declare and validate an end-to-end FailoverSLO before making latency claims.
func HAConfig(sessionID string, exclusive bool) Config {
	cfg := DefaultConfig(sessionID, exclusive)
	cfg.LeaseTTL = 45 * time.Second
	// RenewInterval/RenewJitter/RenewCallTimeout are pinned so the FULL
	// worst-case renew span (now including the per-call timeout, finding H2)
	// stays strictly under the TTL: MaxRenewFails × (RenewInterval +
	// RenewJitter/2 + RenewCallTimeout) = 3 × (10 + 0.5 + 3) = 40.5s < 45s,
	// leaving a 10% margin for clock slack (A9-J5).
	cfg.RenewInterval = 10 * time.Second
	cfg.RenewJitter = 1 * time.Second
	cfg.RenewCallTimeout = 3 * time.Second
	cfg.MaxRenewFails = 3
	cfg.StepDownGrace = 5 * time.Second
	return cfg
}

// deriveRenewInterval returns a renew interval that places the FULL
// MaxRenewFails-th worst-case detection span — interval + jitter/2 + per-call
// renew timeout (finding H2) — at ~75% of the TTL, reserving ~25% of the TTL as
// headroom for clock slack before the lease would expire. This is the
// production derivation path once an operator supplies only LeaseTTL (C3): the
// owner tolerates MaxRenewFails-1 transient renew failures and the recovering
// attempt still lands before the expiry boundary, even when every failing renew
// call burns the full RenewCallTimeout before failing.
func deriveRenewInterval(ttl time.Duration, maxRenewFails int) time.Duration {
	if maxRenewFails < 1 {
		maxRenewFails = 1
	}
	// Budget per attempt for interval + jitter/2 + callTimeout, targeting 75%
	// of the TTL across all MaxRenewFails attempts.
	perAttempt := (ttl * 3) / (time.Duration(maxRenewFails) * 4)
	// Reserve the per-call renew timeout (bounded by its 5s cap) so it does not
	// erode the headroom the way it did before finding H2. Never let the reserve
	// consume more than half the per-attempt budget for tiny TTLs.
	reserve := 5 * time.Second
	if reserve > perAttempt/2 {
		reserve = perAttempt / 2
	}
	// jitter is interval/4 (deriveRenewJitter), so jitter/2 = interval/8. Solve
	// interval + interval/8 + reserve = perAttempt  ⇒  interval = (perAttempt-reserve)*8/9.
	interval := ((perAttempt - reserve) * 8) / 9
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	return interval
}

// deriveRenewJitter returns a jitter derived from the renew interval rather
// than a fixed constant (C3): a quarter of the interval spreads renewals
// enough to avoid a thundering herd while contributing only RenewInterval/8
// per attempt to the worst-case span, comfortably inside the headroom
// reserved by deriveRenewInterval.
func deriveRenewJitter(renewInterval time.Duration) time.Duration {
	j := renewInterval / 4
	if j < 0 {
		j = 0
	}
	return j
}

// deriveAcquirePollInterval returns how often a standby retries Acquire while
// waiting to take over. A standby must poll FASTER than the owner renews so
// failover is bounded by ~LeaseTTL rather than by the owner's renew cadence:
// the smaller of the renew interval and a quarter of the TTL, capped at 5s so
// even large-TTL deployments retry promptly, and floored to avoid a busy loop.
func deriveAcquirePollInterval(renewInterval, ttl time.Duration) time.Duration {
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

// deriveRenewCallTimeout bounds a single Acquire/Renew store call at
// min(RenewInterval/2, 5s), floored at 1s so tiny (test) intervals do not
// create spuriously short deadlines. This stops a hung DynamoDB call from
// stretching step-down and takeover unboundedly (finding H3).
func deriveRenewCallTimeout(renewInterval time.Duration) time.Duration {
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

// renewWorstCaseSpan is the maximum wall-clock time the owner may take to
// detect a definitive lease loss through renewal failures: MaxRenewFails
// attempts, each delayed by the renew interval plus the maximum positive
// jitter (RenewJitter/2) AND the time a single renew call may burn before it
// fails (up to RenewCallTimeout).
//
// renewLoop (manager_lease.go) resets the renew timer AFTER the renew call
// returns, so the real spacing between consecutive attempts is
// interval + jitter/2 + call-duration, and a hung backend burns the full
// RenewCallTimeout on every attempt. Omitting RenewCallTimeout under-counts the
// span by MaxRenewFails × RenewCallTimeout, which in short-TTL profiles can
// push real detection PAST the lease TTL (finding H2/A9-J5). Keeping this
// strictly below LeaseTTL guarantees the owner detects loss and steps down
// before its OWN lease would expire, so it stops sending before a new owner
// takes over (A8-R1 / A9-J5).
func renewWorstCaseSpan(renewInterval, renewJitter, renewCallTimeout time.Duration, maxRenewFails int) time.Duration {
	if maxRenewFails < 1 {
		maxRenewFails = 1
	}
	if renewCallTimeout < 0 {
		renewCallTimeout = 0
	}
	perAttempt := renewInterval + renewJitter/2 + renewCallTimeout
	return perAttempt * time.Duration(maxRenewFails)
}

// clampRenewTimings enforces the expiry-margin invariant defensively at
// construction time: if the worst-case renew span (which now folds in the
// per-call renew timeout, finding H2) meets or exceeds the TTL it sheds jitter
// first (cheap — jitter only spreads load), then, if still unsafe, reserves the
// per-call timeout budget and shrinks the renew interval so the span fits within
// 90% of the TTL. In the pathological case where the per-call timeout alone
// exhausts a per-attempt budget it is clamped too. It returns the (possibly
// adjusted) values and whether any clamp occurred so the caller can warn. This
// is a safety net; Config.Validate reports the same violation as a hard error
// for callers that want to fail fast.
func clampRenewTimings(ttl, renewInterval, renewJitter, renewCallTimeout time.Duration, maxRenewFails int) (time.Duration, time.Duration, time.Duration, bool) {
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
	if renewWorstCaseSpan(renewInterval, renewJitter, renewCallTimeout, maxRenewFails) < ttl {
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
		if renewWorstCaseSpan(renewInterval, renewJitter, renewCallTimeout, maxRenewFails) < ttl {
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

// EffectiveLeaseTTL resolves the lease lifetime exactly as Manager construction
// does. Production validation, activation wiring, and the live manager use this
// one semantic so backend choice cannot change whether a configuration is safe.
func (c Config) EffectiveLeaseTTL() time.Duration {
	if c.LeaseTTL > 0 {
		return c.LeaseTTL
	}
	return DefaultConfig(c.SessionID, c.Exclusive).LeaseTTL
}

// PostAcquireActivationBudget returns the legacy direct-manager fallback used
// when PostAcquireActivationTimeout is zero: one lease lifetime minus the
// reserved teardown margin. Production composition roots install a conservative
// transport bound and renew throughout activation instead of using this window.
func (c Config) PostAcquireActivationBudget() (budget, teardownMargin time.Duration) {
	defaults := DefaultConfig(c.SessionID, c.Exclusive)
	ttl := c.EffectiveLeaseTTL()
	stepDownGrace := c.StepDownGrace
	if stepDownGrace <= 0 {
		stepDownGrace = defaults.StepDownGrace
	}
	teardownMargin = boundedReleaseTimeout(stepDownGrace)
	return ttl - teardownMargin, teardownMargin
}

// EffectiveFailoverLeaseTiming resolves the three lease-side inputs used by
// failover-budget preflight exactly as Manager construction resolves them.
func (c Config) EffectiveFailoverLeaseTiming() (ttl, acquirePoll, renewCallTimeout time.Duration) {
	defaults := DefaultConfig(c.SessionID, c.Exclusive)
	ttl = c.EffectiveLeaseTTL()
	maxFails := c.MaxRenewFails
	if maxFails <= 0 {
		maxFails = defaults.MaxRenewFails
	}
	renewInterval := c.RenewInterval
	renewDerived := renewInterval <= 0
	if renewDerived {
		renewInterval = deriveRenewInterval(ttl, maxFails)
	}
	renewJitter := c.RenewJitter
	if renewJitter < 0 {
		renewJitter = 0
	}
	if renewJitter == 0 && renewDerived {
		renewJitter = deriveRenewJitter(renewInterval)
	}
	renewCallTimeout = c.RenewCallTimeout
	if renewCallTimeout <= 0 {
		renewCallTimeout = deriveRenewCallTimeout(renewInterval)
	}
	renewInterval, _, renewCallTimeout, _ = clampRenewTimings(
		ttl, renewInterval, renewJitter, renewCallTimeout, maxFails,
	)
	acquirePoll = c.AcquirePollInterval
	if acquirePoll <= 0 {
		acquirePoll = deriveAcquirePollInterval(renewInterval, ttl)
	}
	return ttl, acquirePoll, renewCallTimeout
}

// Validate reports whether the lease timings are internally consistent. It is
// intended for callers (e.g. the composition root) that want to fail fast on a
// misconfigured session rather than rely on the manager's defensive clamp.
//
// It rejects negative durations and, when RenewInterval is pinned explicitly,
// any combination whose worst-case jittered renew span — including the per-call
// renew timeout (finding H2) — would reach the TTL: the owner could then fail to
// detect lease loss before its lease expires, permitting two owners to send
// concurrently. Zero-valued knobs are treated as "derive" and are always safe.
func (c Config) Validate() error {
	if c.LeaseTTL < 0 || c.RenewInterval < 0 || c.RenewJitter < 0 ||
		c.StepDownGrace < 0 || c.AcquirePollInterval < 0 || c.RenewCallTimeout < 0 ||
		c.PostAcquireActivationTimeout < 0 || c.FailoverSLO < 0 || c.StartupAllowance < 0 {
		return fmt.Errorf("session %q: lease timings must be non-negative", c.SessionID)
	}
	if c.StartupAllowance > MaxStartupAllowance {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"session %q: StartupAllowance=%s exceeds maximum=%s",
			c.SessionID, c.StartupAllowance, MaxStartupAllowance,
		))
	}
	if c.MaxRenewFails < 0 {
		return fmt.Errorf("session %q: MaxRenewFails must be non-negative", c.SessionID)
	}

	defaults := DefaultConfig(c.SessionID, c.Exclusive)
	ttl := c.EffectiveLeaseTTL()
	if c.Exclusive && ttl < MinimumProductionLeaseTTL {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"session %q: effective LeaseTTL=%s is below production minimum=%s",
			c.SessionID, ttl, MinimumProductionLeaseTTL,
		))
	}
	maxFails := c.MaxRenewFails
	if maxFails == 0 {
		maxFails = defaults.MaxRenewFails
	}

	if c.RenewInterval > 0 {
		callTimeout := c.RenewCallTimeout
		if callTimeout <= 0 {
			callTimeout = deriveRenewCallTimeout(c.RenewInterval)
		}
		worst := renewWorstCaseSpan(c.RenewInterval, c.RenewJitter, callTimeout, maxFails)
		if worst >= ttl {
			return fmt.Errorf(
				"session %q: worst-case renew span %s (MaxRenewFails=%d × (RenewInterval=%s + RenewJitter/2=%s + RenewCallTimeout=%s)) "+
					"must be < LeaseTTL=%s, otherwise the owner cannot detect lease loss before its lease expires",
				c.SessionID, worst, maxFails, c.RenewInterval, c.RenewJitter/2, callTimeout, ttl)
		}
	}
	if c.StepDownGrace >= ttl {
		return fmt.Errorf("session %q: StepDownGrace=%s must be < LeaseTTL=%s", c.SessionID, c.StepDownGrace, ttl)
	}
	return nil
}

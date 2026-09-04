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

	// PerRecordDrainTimeout is the per-record allowance in the outbox drain
	// batch ceiling: min(batchCount * PerRecordDrainTimeout, MaxDrainTimeout).
	// Zero selects the drainer default.
	PerRecordDrainTimeout time.Duration
	// MaxDrainTimeout is the upper bound of that ceiling. Zero selects the
	// drainer default.
	MaxDrainTimeout time.Duration

	// PostAcquireActivationTimeout is the conservative hard bound for the
	// complete initial Start/Reconcile/migration sequence after lease acquisition.
	// Composition roots derive it from the stateful transport's typed timing
	// capability. Zero preserves the direct-manager fallback to the remaining
	// locally safe lease window.
	PostAcquireActivationTimeout time.Duration

	// FailoverSLO is the optional failure-detection to ServiceLevelFull objective.
	// Composition preflight includes two acquire-poll boundaries and the baseline
	// plus every possible minimum-jitter observation Acquire call because call
	// latency after CAS is excluded from persisted elapsed. Zero means no objective is declared.
	FailoverSLO time.Duration

	// StartupAllowance reserves explicit bounded process startup work not already
	// represented by lease, broker-connect, or reconcile durations.
	StartupAllowance time.Duration

	// ConnectAfterLease defers session.Start until the lease is acquired.
	// This avoids connecting to a broker (e.g. MQTT with an exclusive
	// ClientID) before ownership is confirmed, which would disconnect
	// the current owner prematurely.
	ConnectAfterLease bool

	// BrokerHealthStepDown, when > 0, is how long an ACTIVE exclusive owner may
	// stay NON-CONVERGED on its broker path (disconnected, or connected but not
	// re-subscribed) before it voluntarily steps down so a healthy standby can take
	// over (CLUSTER-2). The default lease/renew machinery only fails over on
	// lease/owner/process loss, NOT on a node-local broker outage where the lease
	// store stays reachable and renewals keep succeeding while MQTT reconnects
	// forever — leaving cluster availability down indefinitely. This threshold
	// closes that gap. Zero DISABLES it (the historical behaviour): broker-path
	// failover is opt-in because a globally-unreachable broker would otherwise churn
	// the lease between nodes that all fail to connect. It extends the worst-case
	// failover budget by up to this value, so keep it comfortably above the normal
	// reconnect+reconcile time and validate it against the failover SLO.
	BrokerHealthStepDown time.Duration

	// BrokerPathFailoverDeclared records that an operator STATED the broker-path
	// decision rather than inheriting the disabled default. A positive
	// BrokerHealthStepDown says it on its own; this flag is what carries the
	// explicit opt-out (routing.BrokerPathFailoverOff on the wire). Validate
	// requires one of the two whenever FailoverSLO is declared, so a stated
	// objective can never quietly exclude the node-local broker outage.
	BrokerPathFailoverDeclared bool
}

// DefaultConfig returns a Config with recommended defaults.
//
// RenewInterval is pinned to 110s rather than left zero (which would derive
// LeaseTTL/MaxRenewFails = 120s and place the third renew attempt exactly on
// the 360s expiry boundary). At 110s the three attempts sum to 330s, strictly
// under LeaseTTL, so the owner tolerates two transient renew failures and the
// third (recovering) attempt lands ~30s before expiry instead of racing it
// A custom Config that leaves RenewInterval zero
// still derives LeaseTTL/MaxRenewFails at construction time.
func DefaultConfig(sessionID string, exclusive bool) Config {
	return Config{
		SessionID:           sessionID,
		Exclusive:           exclusive,
		LeaseTTL:            routing.DefaultLeaseTTL,
		RenewInterval:       routing.DefaultRenewInterval, // 110*3=330s < 360s TTL: final renew off the expiry boundary
		RenewJitter:         routing.DefaultRenewJitter,
		MaxRenewFails:       routing.DefaultMaxRenewFails,
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
//     renewLoop resets its timer AFTER each renew call returns:
//     MaxRenewFails × (RenewInterval + RenewJitter/2 + RenewCallTimeout) =
//     3 × (10 + 0.5 + 3) = 40.5s < 45s TTL, so the owner detects a definitive
//     lease loss and steps down with a ~4.5s (10%) margin before its own lease
//     would expire — even when every failing renew call burns the full
//     RenewCallTimeout. (The earlier 14s/2s/no-call-timeout preset summed to
//     58.5s > 45s, i.e. 13.5s PAST expiry;.)
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
	cfg.LeaseTTL = routing.HALeaseTTL
	// RenewInterval/RenewJitter/RenewCallTimeout are pinned so the FULL
	// worst-case renew span (now including the per-call timeout)
	// stays strictly under the TTL: MaxRenewFails × (RenewInterval +
	// RenewJitter/2 + RenewCallTimeout) = 3 × (10 + 0.5 + 3) = 40.5s < 45s,
	// leaving a 10% margin for clock slack.
	cfg.RenewInterval = routing.HARenewInterval
	cfg.RenewJitter = routing.HARenewJitter
	cfg.RenewCallTimeout = routing.HARenewCallTimeout
	cfg.MaxRenewFails = routing.DefaultMaxRenewFails
	cfg.StepDownGrace = routing.HAStepDownGrace
	return cfg
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
	// Through EffectiveStepDownTiming, so this cannot judge the window by a grace
	// the manager will not run: it applies the default substitution AND the
	// below-TTL clamp, where this once applied only the substitution.
	_, teardownMargin = c.EffectiveStepDownTiming()
	return c.EffectiveLeaseTTL() - teardownMargin, teardownMargin
}

// Validate reports whether the lease timings are internally consistent. It is
// intended for callers (e.g. the composition root) that want to fail fast on a
// misconfigured session rather than rely on the manager's defensive clamp.
//
// It rejects negative durations and, when RenewInterval is pinned explicitly,
// any combination whose worst-case jittered renew span — including the per-call
// renew timeout — would reach the TTL: the owner could then fail to
// detect lease loss before its lease expires, permitting two owners to send
// concurrently. Zero-valued knobs mean "derive", which is not the same as safe:
// validateLeaseCadence below resolves them the way the manager does and rejects
// a resolution that does not fit or that no lease store could serve.
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
	if c.Exclusive {
		if err := c.validateLeaseCadence(); err != nil {
			return err
		}
		if err := routing.ValidateBrokerPathPolicy(
			fmt.Sprintf("session %q", c.SessionID), c.FailoverSLO, c.BrokerPathPolicy(),
		); err != nil {
			return err
		}
	}
	return nil
}

// BrokerPathPolicy projects the two broker-path knobs onto the domain decision,
// so every boundary that asks "was this stated, and does it fail over?" asks the
// same question of the same type.
func (c Config) BrokerPathPolicy() routing.BrokerPathPolicy {
	return routing.BrokerPathPolicy{
		StepDown: c.BrokerHealthStepDown,
		Declared: c.BrokerHealthStepDown > 0 || c.BrokerPathFailoverDeclared,
	}
}

// validateLeaseCadence rejects a configuration whose RESOLVED cadence — the one
// the manager will actually run, after derivation and the expiry-margin clamp —
// is unusable, rather than letting construction quietly clamp it and warn.
//
// Two failures reach here that the explicit worst-case span check above cannot
// see, because it only runs when RenewInterval is pinned:
//
//   - The expiry-margin clamp had to cut the renew interval or the per-call
//     timeout. A large MaxRenewFails against a modest TTL leaves no per-attempt
//     budget, so the interval collapses toward the 1 ms floor and the operator's
//     declared failure tolerance is silently not what runs. A clamp that only
//     SHEDS JITTER is not rejected: jitter exists to spread load, the clamp trims
//     it first by design, and what remains is a healthy cadence.
//   - The resolved renew interval or standby acquire poll sits below
//     MinimumLeaseCadence. The owner then renews and every standby claims faster
//     than the store can serve, and the resulting throttling errors are counted
//     as transient renew failures — the store outage becomes a self-inflicted
//     ownership change.
//
// Only exclusive sessions reach this: a non-exclusive session never acquires a
// lease, so its lease knobs are inert.
func (c Config) validateLeaseCadence() error {
	return c.resolveLeaseTiming().ValidateCadence(fmt.Sprintf("session %q", c.SessionID))
}

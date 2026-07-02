package session

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
)

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
	// "derive as LeaseTTL / MaxRenewFails" at construction time.
	RenewInterval time.Duration

	// RenewJitter adds random jitter to each renewal timer to avoid
	// thundering-herd effects. Default: 5s.
	RenewJitter time.Duration

	// MaxRenewFails is the consecutive renewal failures tolerated
	// before the session manager initiates step-down. Default: 3.
	MaxRenewFails int

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

	// ConnectAfterLease defers session.Start until the lease is acquired.
	// This avoids connecting to a broker (e.g. MQTT with an exclusive
	// ClientID) before ownership is confirmed, which would disconnect
	// the current owner prematurely.
	ConnectAfterLease bool
}

// DefaultConfig returns a Config with recommended defaults.
// RenewInterval defaults to zero, which causes the session manager to
// derive it as LeaseTTL / MaxRenewFails at construction time.
func DefaultConfig(sessionID string, exclusive bool) Config {
	return Config{
		SessionID:           sessionID,
		Exclusive:           exclusive,
		LeaseTTL:            360 * time.Second,
		RenewInterval:       0, // derived: LeaseTTL / MaxRenewFails
		RenewJitter:         5 * time.Second,
		MaxRenewFails:       3,
		StepDownGrace:       routing.DefaultStepDownGrace,
		DrainStrategy:       persistence.NewFixedPoll(persistence.DefaultFixedPollInterval),
		DrainBatchSize:      100,
		DrainMaxBatchSize:   500,
		DrainMaxConcurrency: 10,
	}
}

// HAConfig returns a Config tuned for high-availability clusters that need a
// worst-case failover of roughly 45s instead of DefaultConfig's ~360s. It
// starts from DefaultConfig and tightens only the lease-timing knobs, keeping
// the same drain strategy and batch sizes.
//
// Timing: LeaseTTL=45s, MaxRenewFails=3 (so the derived RenewInterval is 15s),
// RenewJitter=2s, StepDownGrace=5s. Those values encode these invariants:
//
//   - Worst-case failover is approximately LeaseTTL (~45s) plus the new
//     owner's acquire+connect time. A standby cannot acquire the Lease until
//     the dead owner's TTL lapses.
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
//   - RenewInterval * MaxRenewFails <= LeaseTTL (15s * 3 = 45s): MaxRenewFails
//     renew attempts fit inside one TTL, so the owner tolerates two
//     consecutive transient renew failures before stepping down.
//   - RenewJitter (2s) stays small relative to the 15s RenewInterval.
//     DefaultConfig's 5s would be a third of the interval and risk late
//     renewals drifting past the TTL.
//
// Tradeoff: faster failover costs ~8x more lease-store renewal writes (every
// ~15s vs ~120s) and tolerates fewer transient renew failures, so blip-prone
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
// The 45s LeaseTTL is a defensible midpoint of the 30-60s HA band; choose a
// value in that band to trade failover speed against renew-write rate and
// blip tolerance.
func HAConfig(sessionID string, exclusive bool) Config {
	cfg := DefaultConfig(sessionID, exclusive)
	cfg.LeaseTTL = 45 * time.Second
	cfg.RenewInterval = 0 // keep derived: LeaseTTL / MaxRenewFails = 15s
	cfg.RenewJitter = 2 * time.Second
	cfg.MaxRenewFails = 3
	cfg.StepDownGrace = 5 * time.Second
	return cfg
}

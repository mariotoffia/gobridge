package session

import (
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// Manager construction: resolving a Config into the timings a term will actually
// run. The cadence itself is resolved by the domain (see config_timing.go) so
// the blueprint validator cannot judge a configuration by values differing from
// these; what stays here is the wiring and the defensive clamp warning.

// NewFromConfig creates a Manager from a Config.
func NewFromConfig(cfg Config, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger) *Manager {
	return newManager(cfg, session, leaseStore, ownerID, logger)
}

// NewWithMetrics creates a Manager from a Config with an explicit
// metrics exporter and clock. Used by composition roots that want to
// pre-wire instrumentation and a deterministic clock before Run.
func NewWithMetrics(cfg Config, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger, metrics ports.MetricsExporter, clk clock.Clock) *Manager {
	mgr := newManager(cfg, session, leaseStore, ownerID, logger)
	if metrics != nil {
		mgr.metrics = metrics
	}
	if clk != nil {
		mgr.clk = clk
	}
	return mgr
}

func newManager(cfg Config, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger) *Manager {
	defaults := DefaultConfig(cfg.SessionID, cfg.Exclusive)
	cfg.LeaseTTL = cfg.EffectiveLeaseTTL()
	if cfg.MaxRenewFails <= 0 {
		cfg.MaxRenewFails = defaults.MaxRenewFails
	}
	// Derive the renewal cadence from the TTL when the operator supplies only
	// LeaseTTL (bridge/convert.go no longer seeds DefaultConfig, so this is
	// now the production path). deriveRenewInterval/deriveRenewJitter target the
	// MaxRenewFails-th renew at ~75% of the TTL, folding jitter into the
	// expiry-margin invariant so renew×maxFails+jitter < ttl with margin.
	//
	// Jitter is derived ONLY when the renew interval was also unset. If the
	// caller pinned RenewInterval it is explicit enough that a zero RenewJitter
	// is honored as "no jitter" (deterministic cadence) rather than reinterpreted
	// as "derive"; an operator wanting spread on a pinned interval sets the
	// lease_renew_jitter field. The production path leaves both zero, so both
	// are derived.
	renewIntervalDerived := cfg.RenewInterval <= 0
	if renewIntervalDerived {
		cfg.RenewInterval = deriveRenewInterval(cfg.LeaseTTL, cfg.MaxRenewFails)
	}
	if cfg.RenewJitter < 0 {
		cfg.RenewJitter = 0
	}
	if cfg.RenewJitter == 0 && renewIntervalDerived {
		cfg.RenewJitter = deriveRenewJitter(cfg.RenewInterval)
	}
	// Resolve RenewCallTimeout BEFORE the expiry-margin clamp so it participates
	// in the worst-case renew span: renewLoop resets its timer
	// AFTER each renew call, so a hung call's full RenewCallTimeout adds to the
	// spacing between attempts and must be counted before deciding whether the
	// timings fit under the TTL.
	if cfg.RenewCallTimeout <= 0 {
		cfg.RenewCallTimeout = deriveRenewCallTimeout(cfg.RenewInterval)
	}
	// Defensively enforce the expiry-margin invariant even for explicit configs
	// so a hand-tuned combination can never produce a renew span (interval +
	// jitter/2 + call-timeout) that reaches the TTL (Config.Validate reports the
	// same violation as a hard error).
	renewInterval, renewJitter, renewCallTimeout, clamped := clampRenewTimings(cfg.LeaseTTL, cfg.RenewInterval, cfg.RenewJitter, cfg.RenewCallTimeout, cfg.MaxRenewFails)
	if clamped && logger != nil {
		logger.Warn("session lease timings clamped to satisfy the expiry-margin invariant",
			"session_id", cfg.SessionID,
			"lease_ttl", cfg.LeaseTTL,
			"requested_renew_interval", cfg.RenewInterval,
			"requested_renew_jitter", cfg.RenewJitter,
			"requested_renew_call_timeout", cfg.RenewCallTimeout,
			"clamped_renew_interval", renewInterval,
			"clamped_renew_jitter", renewJitter,
			"clamped_renew_call_timeout", renewCallTimeout,
			"max_renew_fails", cfg.MaxRenewFails,
		)
	}
	cfg.RenewInterval = renewInterval
	cfg.RenewJitter = renewJitter
	cfg.RenewCallTimeout = renewCallTimeout
	if cfg.AcquirePollInterval <= 0 {
		cfg.AcquirePollInterval = deriveAcquirePollInterval(cfg.RenewInterval, cfg.LeaseTTL)
	}
	if cfg.StepDownGrace <= 0 {
		cfg.StepDownGrace = defaults.StepDownGrace
	}
	// StepDownGrace must stay strictly below LeaseTTL: a stepping-down owner
	// drains in-flight Send+Complete for StepDownGrace BEFORE releasing, so a
	// grace at or above the TTL lets the old owner keep draining PAST its own
	// lease expiry and overlap the new owner. Config.Validate rejects this as a
	// hard error; here — mirroring clampRenewTimings — newManager (which returns
	// no error) clamps defensively and warns so a hand-tuned or programmatic
	// Config can never breach the invariant on the construction path.
	//
	// This bound limits the DURATION of any owner overlap; it is NOT what makes
	// single-active safe. Correctness comes from monotonic fencing: a stale
	// owner's Send/Complete carry an outdated fencing version the stores reject,
	// so even an overlapping drain cannot double-process. Keeping grace < TTL
	// simply keeps that (already-safe) overlap window short, minimising duplicate
	// egress during a handover.
	//
	// ponytail: clamp to LeaseTTL/2, an obviously-sub-TTL drain window, rather
	// than TTL-ε; the exact clamped value is not load-bearing — only that it is
	// well under the TTL. Operators wanting a precise grace should pass a valid
	// (StepDownGrace < LeaseTTL) config, which is left untouched.
	if cfg.StepDownGrace >= cfg.LeaseTTL {
		clampedGrace := cfg.LeaseTTL / 2
		if logger != nil {
			logger.Warn("session StepDownGrace clamped below LeaseTTL to keep step-down within the lease",
				"session_id", cfg.SessionID,
				"requested_step_down_grace", cfg.StepDownGrace,
				"lease_ttl", cfg.LeaseTTL,
				"clamped_step_down_grace", clampedGrace,
			)
		}
		cfg.StepDownGrace = clampedGrace
	}

	return &Manager{
		sessionID:            cfg.SessionID,
		session:              session,
		leaseStore:           leaseStore,
		ownerID:              ownerID,
		exclusive:            cfg.Exclusive,
		connectAfterLease:    cfg.ConnectAfterLease,
		plan:                 cfg.Plan,
		leaseTTL:             cfg.LeaseTTL,
		brokerHealthStepDown: cfg.BrokerHealthStepDown,
		renewInterval:        cfg.RenewInterval,
		renewJitter:          cfg.RenewJitter,
		acquirePoll:          cfg.AcquirePollInterval,
		renewCallTimeout:     cfg.RenewCallTimeout,
		maxRenewFails:        cfg.MaxRenewFails,
		stepDownGrace:        cfg.StepDownGrace,
		activationTimeout:    cfg.PostAcquireActivationTimeout,
		metrics:              &ports.NoopExporter{},
		audit:                ports.NoopAuditLogger{},
		logger:               logger,
		clk:                  clock.System,
		leaseEvents:          make(chan LeaseStateEvent, leaseEventBuffer),
	}
}

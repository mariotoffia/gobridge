package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
)

// ValidationError is the shape the config package returns from
// Validate / ValidateWithWarnings. It is an alias for the
// transport-neutral ports.BlueprintValidationError so admin layers
// (httpapi) can inspect Warnings/Errors without importing this
// package.
type ValidationError = ports.BlueprintValidationError

// Validate performs structural validation on a ports.BridgeConfig. It
// checks required fields, valid enum values, and duration formats at
// the field level, then delegates cross-reference and route-graph
// validation (FK integrity between sessions, receivers, senders,
// bindings, routes, resolver bindings, and clustered MQTT shared-
// subscription rules) to the validate package.
//
// It does not check transport-specific options.
func Validate(cfg *ports.BridgeConfig) error {
	ve := validateConfig(cfg)
	if ve.HasErrors() {
		return ve
	}
	return nil
}

// ValidateWithWarnings performs Validate and returns any non-fatal
// warnings (e.g. direct_hold fencing advisory) even when validation
// passes. The caller should log warnings but not treat them as errors.
func ValidateWithWarnings(cfg *ports.BridgeConfig) (warnings []string, err error) {
	ve := validateConfig(cfg)
	if ve.HasErrors() {
		return ve.Warnings, ve
	}
	return ve.Warnings, nil
}

func validateConfig(cfg *ports.BridgeConfig) *ValidationError {
	ve := &ValidationError{}

	validateBridgeFields(ve, cfg)
	validateConfigWatchFields(ve, cfg)

	if graphErr := validate.ValidateBlueprintGraph(cfg); graphErr != nil {
		ve.Errors = append(ve.Errors, graphErr.Errors...)
		ve.Warnings = append(ve.Warnings, graphErr.Warnings...)
	}

	validateStaleClaimDuration(ve, cfg)
	validateSessionRenewTiming(ve, cfg)

	return ve
}

// validateSessionRenewTiming enforces the renew/lease invariant for every
// route session that sets renew_interval explicitly:
//
//	(renewInterval + jitter/2 + renewCallTimeout) * maxRenewFails < leaseTTL
//
// If it does not hold, the owner can burn through all its tolerated renewal
// failures (plus worst-case jitter AND the time each renew store call may burn
// before it fails) before the lease-loss is detected — the lease can lapse and
// a standby take over while the old owner still believes it holds it, risking
// split-brain. The per-attempt renewCallTimeout term matters because renewLoop
// resets its timer AFTER the renew call returns, so a hung backend adds up to
// renew_call_timeout to every attempt (finding H2). This mirrors the invariant
// runtime/session Config.Validate enforces (renewWorstCaseSpan); duplicating it
// in the config layer fails a statically-rejectable blueprint before any
// runtime is built (contract C3). renew_interval left empty is derived
// downstream and is safe, so it is not checked here; lease_ttl empty falls back
// to the runtime default.
func validateSessionRenewTiming(ve *ValidationError, cfg *ports.BridgeConfig) {
	// Mirrors runtime/session.DefaultConfig defaults so the config-layer check
	// rejects exactly the set the runtime would (never more, never less). The
	// config package must not import runtime/session (layering), so the
	// literals are duplicated with this note.
	const (
		defaultMaxRenewFails = 3
		defaultLeaseTTL      = 360 * time.Second
	)
	for _, r := range cfg.Routes {
		s := r.Session
		if s == nil || s.RenewInterval == "" {
			continue
		}
		renew, err := time.ParseDuration(s.RenewInterval)
		if err != nil {
			ve.Addf("routes[%s].session.renew_interval: invalid duration %q: %v", r.ID, s.RenewInterval, err)
			continue
		}
		if renew <= 0 {
			continue
		}
		lease := defaultLeaseTTL
		if s.LeaseTTL != "" {
			lease, err = time.ParseDuration(s.LeaseTTL)
			if err != nil {
				ve.Addf("routes[%s].session.lease_ttl: invalid duration %q: %v", r.ID, s.LeaseTTL, err)
				continue
			}
		}
		var jitter time.Duration
		if s.RenewJitter != "" {
			jitter, err = time.ParseDuration(s.RenewJitter)
			if err != nil {
				ve.Addf("routes[%s].session.lease_renew_jitter: invalid duration %q: %v", r.ID, s.RenewJitter, err)
				continue
			}
		}
		maxFails := s.MaxRenewFails
		if maxFails <= 0 {
			maxFails = defaultMaxRenewFails
		}
		// renew_call_timeout is an operator-settable blueprint field (finding
		// H2 folded it into the safety invariant). When unset/zero the runtime
		// derives it as min(renewInterval/2, 5s) floored at 1s
		// (runtime/session.deriveRenewCallTimeout); the literals below duplicate
		// that derivation so the config-layer worst case matches the runtime's.
		callTimeout, err := parseRenewCallTimeout(s, renew)
		if err != nil {
			ve.Addf("routes[%s].session.renew_call_timeout: invalid duration %q: %v", r.ID, s.RenewCallTimeout, err)
			continue
		}
		worst := (renew + jitter/2 + callTimeout) * time.Duration(maxFails)
		if worst >= lease {
			ve.Addf("routes[%s].session: worst-case renew span %s "+
				"((renew_interval=%s + lease_renew_jitter/2=%s + renew_call_timeout=%s) * max_renew_fails=%d) must be < "+
				"lease_ttl (%s); otherwise the owner can exhaust all tolerated renewal failures before "+
				"the lease expires, risking split-brain takeover — lower renew_interval/max_renew_fails/renew_call_timeout "+
				"or raise lease_ttl",
				r.ID, worst, renew, jitter/2, callTimeout, maxFails, lease)
		}
	}
}

// parseRenewCallTimeout returns the per-attempt renew-call-timeout term used in
// the worst-case span. It parses the operator-set renew_call_timeout, or, when
// unset/zero, duplicates runtime/session.deriveRenewCallTimeout's fallback
// (min(renewInterval/2, 5s) floored at 1s) so the config-layer invariant folds
// in exactly the term the runtime uses. The config package must not import
// runtime/session (layering), so the literals are duplicated with this note.
func parseRenewCallTimeout(s *ports.RouteSessionDef, renewInterval time.Duration) (time.Duration, error) {
	const (
		maxDerivedTimeout = 5 * time.Second
		minDerivedTimeout = 1 * time.Second
	)
	if s.RenewCallTimeout != "" {
		ct, err := time.ParseDuration(s.RenewCallTimeout)
		if err != nil {
			return 0, fmt.Errorf("parse renew_call_timeout %q: %w", s.RenewCallTimeout, err)
		}
		if ct > 0 {
			return ct, nil
		}
	}
	t := renewInterval / 2
	if t > maxDerivedTimeout {
		t = maxDerivedTimeout
	}
	if t < minDerivedTimeout {
		t = minDerivedTimeout
	}
	return t, nil
}

func validateBridgeFields(ve *ValidationError, cfg *ports.BridgeConfig) {
	if cfg.Bridge.ID == "" {
		ve.Add("bridge.id is required")
	}

	if cfg.Bridge.DeploymentMode != "" {
		validateEnum(ve, "bridge.deployment_mode", cfg.Bridge.DeploymentMode,
			"standalone", "clustered")
	}

	if cfg.Bridge.ShutdownTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.ShutdownTimeout); err != nil {
			ve.Addf("bridge.shutdown_timeout: invalid duration %q: %v", cfg.Bridge.ShutdownTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.shutdown_timeout: must be positive, got %s", cfg.Bridge.ShutdownTimeout)
		}
	}
	if cfg.Bridge.DrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.DrainTimeout); err != nil {
			ve.Addf("bridge.drain_timeout: invalid duration %q: %v", cfg.Bridge.DrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.drain_timeout: must be positive, got %s", cfg.Bridge.DrainTimeout)
		}
	}
	var perRecordDrain, maxDrain time.Duration
	if cfg.Bridge.PerRecordDrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.PerRecordDrainTimeout); err != nil {
			ve.Addf("bridge.per_record_drain_timeout: invalid duration %q: %v", cfg.Bridge.PerRecordDrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.per_record_drain_timeout: must be positive, got %s", cfg.Bridge.PerRecordDrainTimeout)
		} else {
			perRecordDrain = d
		}
	}
	if cfg.Bridge.MaxDrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.MaxDrainTimeout); err != nil {
			ve.Addf("bridge.max_drain_timeout: invalid duration %q: %v", cfg.Bridge.MaxDrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.max_drain_timeout: must be positive, got %s", cfg.Bridge.MaxDrainTimeout)
		} else {
			maxDrain = d
		}
	}
	if perRecordDrain > 0 && maxDrain > 0 && maxDrain < perRecordDrain {
		ve.Addf("bridge.max_drain_timeout (%s) must be >= per_record_drain_timeout (%s)",
			cfg.Bridge.MaxDrainTimeout, cfg.Bridge.PerRecordDrainTimeout)
	}
}

func validateConfigWatchFields(ve *ValidationError, cfg *ports.BridgeConfig) {
	cw := cfg.ConfigWatch
	if cw == nil {
		return
	}
	if cw.Mode != "" {
		validateEnum(ve, "config_watch.mode", cw.Mode, "notify", "poll")
	}
	if cw.PollInterval != "" {
		if d, err := time.ParseDuration(cw.PollInterval); err != nil {
			ve.Addf("config_watch.poll_interval: invalid duration %q: %v", cw.PollInterval, err)
		} else if d <= 0 {
			ve.Addf("config_watch.poll_interval: must be positive, got %s", cw.PollInterval)
		}
	}
	if cw.Debounce != "" {
		if d, err := time.ParseDuration(cw.Debounce); err != nil {
			ve.Addf("config_watch.debounce: invalid duration %q: %v", cw.Debounce, err)
		} else if d <= 0 {
			ve.Addf("config_watch.debounce: must be positive, got %s", cw.Debounce)
		}
	}
}

func validateEnum(ve *ValidationError, field, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	ve.Addf("%s: invalid value %q, must be one of: %s", field, value, strings.Join(allowed, ", "))
}

// validateStaleClaimDuration warns when the outbox store's
// stale_claim_duration is explicitly set to a value much larger than the
// session step-down grace periods. The real lower bound on
// stale_claim_duration is the worst-case drain-batch timeout: a stranded
// claim must exceed it before the SAME owner can reclaim it (this recovery
// path is DynamoDB-outbox-only). A value far above that ceiling only delays
// same-owner stranded-claim recovery — it does not gate failover hand-off,
// which a new owner drives immediately via its strictly higher fencing
// version. The 2x-max-StepDownGrace threshold is a cheap proxy for "much
// larger than the drain ceiling"; it does not change the recovery semantics.
func validateStaleClaimDuration(ve *ValidationError, cfg *ports.BridgeConfig) {
	if cfg.Stores.Outbox == nil {
		return
	}
	opts := rawMap(cfg.Stores.Outbox.Raw())
	if opts == nil {
		return
	}
	raw, ok := opts["stale_claim_duration"]
	if !ok {
		return
	}

	var stale time.Duration
	switch v := raw.(type) {
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			ve.Addf("stores.outbox.options.stale_claim_duration: invalid duration %q: %v", v, err)
			return
		}
		stale = d
	case time.Duration:
		stale = v
	default:
		ve.Addf("stores.outbox.options.stale_claim_duration: must be a duration string (e.g. \"30s\"), got %T", raw)
		return
	}

	if stale <= 0 {
		ve.Addf("stores.outbox.options.stale_claim_duration: must be positive, got %v", stale)
		return
	}

	maxGrace := routing.DefaultStepDownGrace
	for _, r := range cfg.Routes {
		if r.Session == nil {
			continue
		}
		grace := routing.DefaultStepDownGrace
		if r.Session.StepDownGrace != "" {
			if d, err := time.ParseDuration(r.Session.StepDownGrace); err == nil {
				grace = d
			}
		}
		if grace > maxGrace {
			maxGrace = grace
		}
	}

	if stale > 2*maxGrace {
		ve.Warnf("stores.outbox.options.stale_claim_duration (%s) is more than 2x "+
			"the maximum step_down_grace (%s); a value this large only delays "+
			"same-owner stranded-claim recovery (DynamoDB outbox only) and does "+
			"not gate failover hand-off. The real lower bound is the worst-case "+
			"drain-batch timeout, which a stranded claim must exceed before it "+
			"can be reclaimed; step_down_grace + 15s (%s) is only a rule-of-thumb "+
			"starting point that usually clears that ceiling, not the constraint",
			stale, maxGrace, maxGrace+15*time.Second)
	}
}

package config

import (
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

	return ve
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

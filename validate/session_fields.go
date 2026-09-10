package validate

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// Field-level rules for a route's session block that the BUILDER also enforces.
// They live in the validator because the admin config transaction validates,
// writes durably, and only then applies: a rule enforced solely at build time
// turns a typo into a committed config plus a failed apply plus a rollback.
// The lease CADENCE rule, which needs the whole resolution rather than one
// field, is in session_lease_cadence.go.

// validateSessionDurationFields checks the foreign-key-adjacent
// duration fields on a route's session block. Pure field-level
// duration parsing for top-level sections (bridge.*, config_watch.*)
// remains in the config package.
func validateSessionDurationFields(ve *ports.BlueprintValidationError, prefix string, sess *ports.RouteSessionDef) {
	for _, f := range []struct{ name, val string }{
		{"lease_ttl", sess.LeaseTTL},
		{"renew_interval", sess.RenewInterval},
		{"step_down_grace", sess.StepDownGrace},
	} {
		if f.val == "" {
			continue
		}
		d, err := time.ParseDuration(f.val)
		if err != nil {
			ve.Addf("%s: session.%s: invalid duration %q: %v", prefix, f.name, f.val, err)
		} else if d <= 0 {
			ve.Addf("%s: session.%s: must be positive, got %s", prefix, f.name, f.val)
		}
	}
	if sess.MaxRenewFails < 0 {
		ve.Addf("%s: session.max_renew_fails: must be non-negative, got %d", prefix, sess.MaxRenewFails)
	}
	validateSessionBrokerPathPolicy(ve, prefix, sess)

	// Build-time-consumed session duration fields (bridge/convert.go
	// toSessionConfigE / toDrainStrategyE) that were previously parsed ONLY at
	// build time — validate them here too so a bad value fails validation
	// instead of escaping to a restart-time apply failure. These mirror the builder's parse
	// exactly (reject only an unparseable duration) so no value the builder
	// would accept is rejected here. drain_interval is also guarded for
	// mutual-exclusion with drain_strategy in validateRouteDrainStrategy.
	for _, f := range []struct{ name, val string }{
		{"lease_renew_jitter", sess.RenewJitter},
		{"acquire_poll_interval", sess.AcquirePollInterval},
		{"renew_call_timeout", sess.RenewCallTimeout},
		{"drain_interval", sess.DrainInterval},
	} {
		if f.val == "" {
			continue
		}
		if _, err := time.ParseDuration(f.val); err != nil {
			ve.Addf("%s: session.%s: invalid duration %q: %v", prefix, f.name, f.val, err)
		}
	}
}

func validateRouteDrainStrategy(ve *ports.BlueprintValidationError, prefix string, sess *ports.RouteSessionDef) {
	ds := sess.DrainStrategy
	if ds == nil {
		return
	}

	if sess.DrainInterval != "" {
		ve.Addf("%s: session.drain_strategy and session.drain_interval are mutually exclusive", prefix)
	}

	field := prefix + ": session.drain_strategy"

	switch ds.Type {
	case "fixed_poll":
		if ds.Interval != "" {
			if d, err := time.ParseDuration(ds.Interval); err != nil {
				ve.Addf("%s: invalid interval %q: %v", field, ds.Interval, err)
			} else if d <= 0 {
				ve.Addf("%s: interval must be positive, got %s", field, ds.Interval)
			}
		}

	case "adaptive_backoff":
		var minD, maxD time.Duration
		if ds.MinInterval != "" {
			d, err := time.ParseDuration(ds.MinInterval)
			if err != nil {
				ve.Addf("%s: invalid min_interval %q: %v", field, ds.MinInterval, err)
			}
			minD = d
		}
		if ds.MaxInterval != "" {
			d, err := time.ParseDuration(ds.MaxInterval)
			if err != nil {
				ve.Addf("%s: invalid max_interval %q: %v", field, ds.MaxInterval, err)
			}
			maxD = d
		}
		if minD > 0 && maxD > 0 && maxD < minD {
			ve.Addf("%s: max_interval (%s) must be >= min_interval (%s)", field, ds.MaxInterval, ds.MinInterval)
		}
		if ds.Multiplier != 0 && ds.Multiplier <= 1.0 {
			ve.Addf("%s: multiplier must be > 1.0, got %v", field, ds.Multiplier)
		}

	default:
		ve.Addf("%s: invalid type %q, must be one of: fixed_poll, adaptive_backoff", field, ds.Type)
	}
}

// validateSessionBrokerPathPolicy judges the broker-path failover decision
// before the durable write, through the same domain rule the builder and the
// session manager use. broker_health_step_down is TRI-state — empty leaves the
// decision unmade, routing.BrokerPathFailoverOff is an explicit decision not to
// fail over on a node-local broker outage, and a positive duration enables it —
// so it cannot go through the plain duration loop above.
func validateSessionBrokerPathPolicy(ve *ports.BlueprintValidationError, prefix string, sess *ports.RouteSessionDef) {
	policy, err := routing.ParseBrokerPathPolicy(sess.BrokerHealthStepDown)
	if err != nil {
		ve.Addf("%s: session.%v", prefix, err)
		return
	}
	slo, sloErr := time.ParseDuration(sess.FailoverSLO)
	if sess.FailoverSLO == "" || sloErr != nil {
		// An unparseable failover_slo is already reported by the config layer;
		// judging the policy against a value the operator never wrote would only
		// add a confusing second error.
		return
	}
	if err := routing.ValidateBrokerPathPolicy(prefix+": session", slo, policy); err != nil {
		ve.Addf("%v", err)
	}
}

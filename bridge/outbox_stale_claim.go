package bridge

import (
	"fmt"
	"math"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// derivedStaleClaimDuration returns the stale-claim duration a build of cfg
// hands its outbox store factory; ok is false when cfg configures no outbox
// store, so no store is handed one.
//
// The store takes the value when it is OPENED. An in-place reload keeps the
// running store open, so it is only eligible while this value is unchanged —
// and it asks this function, the one the builder asks, so the two cannot
// disagree.
func derivedStaleClaimDuration(cfg *ports.BridgeConfig) (time.Duration, bool, error) {
	if cfg == nil || cfg.Stores.Outbox == nil {
		return 0, false, nil
	}
	d, err := staleClaimDuration(cfg, cfg.Stores.Outbox)
	return d, true, err
}

// staleClaimDuration derives the outbox reclaim timeout for the outbox store
// sc configures in a build of cfg, in this priority:
//
//  1. an explicit `stale_claim_duration` entry in the outbox YAML
//     options (read via StoreConfig.Raw()) — supports either a
//     duration string ("2m") or a time.Duration value;
//  2. a value derived from the maximum session step-down grace
//     across all routes, plus a buffer.
//
// The derivation keeps the outbox reclaim timeout aligned with the
// lease lifecycle without forcing every plugin config schema to
// carry the runtime knob.
func staleClaimDuration(cfg *ports.BridgeConfig, sc *ports.StoreConfig) (time.Duration, error) {
	if explicit, ok, err := explicitStaleClaimDuration(sc); err != nil {
		return 0, err
	} else if ok {
		return explicit, nil
	}

	maxStepDownGrace := session.DefaultConfig("", true).StepDownGrace
	for _, r := range cfg.Routes {
		if r.Session == nil {
			continue
		}
		sessCfg, err := toSessionConfigE(r.Session, IsClusteredDeployment(cfg))
		if err != nil {
			return 0, fmt.Errorf("bridge: route %q: %w", r.ID, err)
		}
		if sessCfg != nil && sessCfg.StepDownGrace > maxStepDownGrace {
			maxStepDownGrace = sessCfg.StepDownGrace
		}
	}

	// A grace near time.Duration's maximum is valid configuration, and plain
	// arithmetic would wrap it into a negative timeout; saturating keeps an
	// absurd grace an absurd (never-reclaim) timeout instead.
	staleClaimBuffer := max(saturatingAdd(maxStepDownGrace, maxStepDownGrace), 15*time.Second)
	return saturatingAdd(maxStepDownGrace, staleClaimBuffer), nil
}

// saturatingAdd returns a+b for non-negative a and b, clamped at the largest
// time.Duration rather than wrapping negative.
func saturatingAdd(a, b time.Duration) time.Duration {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// explicitStaleClaimDuration looks for a user-provided override in
// the outbox blueprint's raw stage-1 options. It returns ok=false
// when the override is absent.
func explicitStaleClaimDuration(sc *ports.StoreConfig) (time.Duration, bool, error) {
	raw := sc.Raw()
	if raw == nil {
		return 0, false, nil
	}
	var probe struct {
		StaleClaimDuration any `mapstructure:"stale_claim_duration" yaml:"stale_claim_duration" json:"stale_claim_duration"`
	}
	if err := raw.Decode(&probe); err != nil {
		return 0, false, nil
	}
	switch v := probe.StaleClaimDuration.(type) {
	case nil:
		return 0, false, nil
	case time.Duration:
		return v, true, nil
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, false, fmt.Errorf("bridge: outbox stale_claim_duration: invalid duration %q: %w", v, err)
		}
		return d, true, nil
	default:
		return 0, false, fmt.Errorf("bridge: outbox stale_claim_duration: must be a duration string or time.Duration, got %T", v)
	}
}

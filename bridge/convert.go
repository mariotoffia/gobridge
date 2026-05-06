package bridge

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

func toRoutePolicy(r ports.RouteDef) routing.RoutePolicy {
	p, _ := toRoutePolicyE(r)
	return p
}

func toRoutePolicyE(r ports.RouteDef) (routing.RoutePolicy, error) {
	p := routing.RoutePolicy{
		DeliveryMode:       routing.DeliveryMode(r.DeliveryMode),
		DispatchMode:       routing.DispatchMode(r.DispatchMode),
		MaxInFlight:        r.Policy.MaxInFlight,
		MaxReplayAttempts:  r.Policy.MaxReplayAttempts,
		MaxOutboxDepth:     r.Policy.MaxOutboxDepth,
		AckAfter:           routing.AckBoundary(r.Policy.AckAfter),
		OnExpired:          routing.ExpiredAction(r.Policy.OnExpired),
		OnPermanentFailure: routing.FailureAction(r.Policy.OnPermanentFailure),
		AllowUnfenced:      r.Policy.AllowUnfenced,
		AllowRetryDrop:     r.Policy.AllowRetryDrop,
	}
	if r.Policy.SendTimeout != "" {
		d, err := time.ParseDuration(r.Policy.SendTimeout)
		if err != nil {
			return p, fmt.Errorf("invalid send_timeout %q: %w", r.Policy.SendTimeout, err)
		}
		p.SendTimeout = d
	}
	if r.Policy.DepthCacheTTL != "" {
		d, err := time.ParseDuration(r.Policy.DepthCacheTTL)
		if err != nil {
			return p, fmt.Errorf("invalid depth_cache_ttl %q: %w", r.Policy.DepthCacheTTL, err)
		}
		p.DepthCacheTTL = d
	}
	if r.Policy.Backoff.InitialInterval != "" {
		d, err := time.ParseDuration(r.Policy.Backoff.InitialInterval)
		if err != nil {
			return p, fmt.Errorf("invalid backoff initial_interval %q: %w", r.Policy.Backoff.InitialInterval, err)
		}
		p.Backoff.InitialInterval = d
	}
	if r.Policy.Backoff.MaxInterval != "" {
		d, err := time.ParseDuration(r.Policy.Backoff.MaxInterval)
		if err != nil {
			return p, fmt.Errorf("invalid backoff max_interval %q: %w", r.Policy.Backoff.MaxInterval, err)
		}
		p.Backoff.MaxInterval = d
	}
	if r.Policy.Backoff.Multiplier != 0 {
		p.Backoff.Multiplier = r.Policy.Backoff.Multiplier
	}
	return p, nil
}

func toSessionConfig(rs *ports.RouteSessionDef) *runtime.SessionConfig {
	sc, _ := toSessionConfigE(rs)
	return sc
}

func toSessionConfigE(rs *ports.RouteSessionDef) (*runtime.SessionConfig, error) {
	if rs == nil {
		return nil, nil
	}

	sc := runtime.DefaultSessionConfig(rs.SessionID, true)
	sc.ConnectAfterLease = rs.ConnectAfterLease

	if rs.LeaseTTL != "" {
		d, err := time.ParseDuration(rs.LeaseTTL)
		if err != nil {
			return nil, fmt.Errorf("invalid lease_ttl %q: %w", rs.LeaseTTL, err)
		}
		sc.LeaseTTL = d
	}
	if rs.RenewInterval != "" {
		d, err := time.ParseDuration(rs.RenewInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid renew_interval %q: %w", rs.RenewInterval, err)
		}
		sc.RenewInterval = d
	}
	if rs.MaxRenewFails > 0 {
		sc.MaxRenewFails = rs.MaxRenewFails
	}
	if rs.StepDownGrace != "" {
		d, err := time.ParseDuration(rs.StepDownGrace)
		if err != nil {
			return nil, fmt.Errorf("invalid step_down_grace %q: %w", rs.StepDownGrace, err)
		}
		sc.StepDownGrace = d
	}

	ds, err := toDrainStrategyE(rs)
	if err != nil {
		return nil, err
	}
	sc.DrainStrategy = ds
	if rs.DrainBatchSize > 0 {
		sc.DrainBatchSize = rs.DrainBatchSize
	}
	if rs.DrainMaxBatchSize > 0 {
		sc.DrainMaxBatchSize = rs.DrainMaxBatchSize
	}
	if rs.DrainMaxConcurrency > 0 {
		sc.DrainMaxConcurrency = rs.DrainMaxConcurrency
	}

	return &sc, nil
}

// applyBridgeDrainDefaults copies bridge-level drain timeout settings
// into a session config when the session does not already set them.
// This keeps the drain ceiling configurable from a single
// YAML location while preserving per-session overrides.
func applyBridgeDrainDefaults(sc *runtime.SessionConfig, bs ports.BridgeSettings) {
	if sc == nil {
		return
	}
	if sc.DrainTimeout == 0 {
		if d := bs.DrainTimeoutDuration(); d > 0 && bs.DrainTimeout != "" {
			sc.DrainTimeout = d
		}
	}
	if sc.PerRecordDrainTimeout == 0 {
		sc.PerRecordDrainTimeout = bs.PerRecordDrainTimeoutDuration()
	}
	if sc.MaxDrainTimeout == 0 {
		sc.MaxDrainTimeout = bs.MaxDrainTimeoutDuration()
	}
}

func toDrainStrategy(rs *ports.RouteSessionDef) persistence.DrainStrategy {
	ds, _ := toDrainStrategyE(rs)
	return ds
}

func toDrainStrategyE(rs *ports.RouteSessionDef) (persistence.DrainStrategy, error) {
	if rs.DrainStrategy != nil {
		return buildDrainStrategyE(rs.DrainStrategy)
	}
	if rs.DrainInterval != "" {
		d, err := time.ParseDuration(rs.DrainInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid drain_interval %q: %w", rs.DrainInterval, err)
		}
		return persistence.NewFixedPoll(d), nil
	}
	return persistence.NewFixedPoll(persistence.DefaultFixedPollInterval), nil
}

func buildDrainStrategyE(ds *ports.DrainStrategyDef) (persistence.DrainStrategy, error) {
	switch ds.Type {
	case "adaptive_backoff":
		var minD, maxD time.Duration
		if ds.MinInterval != "" {
			d, err := time.ParseDuration(ds.MinInterval)
			if err != nil {
				return nil, fmt.Errorf("invalid adaptive_backoff min_interval %q: %w", ds.MinInterval, err)
			}
			minD = d
		}
		if ds.MaxInterval != "" {
			d, err := time.ParseDuration(ds.MaxInterval)
			if err != nil {
				return nil, fmt.Errorf("invalid adaptive_backoff max_interval %q: %w", ds.MaxInterval, err)
			}
			maxD = d
		}
		return persistence.NewAdaptiveBackoff(minD, maxD, ds.Multiplier), nil

	default:
		var interval time.Duration
		if ds.Interval != "" {
			d, err := time.ParseDuration(ds.Interval)
			if err != nil {
				return nil, fmt.Errorf("invalid fixed_poll interval %q: %w", ds.Interval, err)
			}
			interval = d
		}
		return persistence.NewFixedPoll(interval), nil
	}
}

func toBindings(cfg *ports.BridgeConfig, bindingIDs []string) []routing.DestinationBinding {
	out := make([]routing.DestinationBinding, 0, len(bindingIDs))
	for _, id := range bindingIDs {
		bd := findBinding(cfg, id)
		if bd == nil {
			continue
		}
		out = append(out, routing.DestinationBinding{
			ID:        bd.ID,
			SessionID: bd.SessionID,
			SenderID:  bd.SenderID,
			Address:   bd.Address,
			Config:    bd.Config,
		})
	}
	return out
}

func findBinding(cfg *ports.BridgeConfig, id string) *ports.BindingDef {
	for i := range cfg.Bindings {
		if cfg.Bindings[i].ID == id {
			return &cfg.Bindings[i]
		}
	}
	return nil
}

func findSession(cfg *ports.BridgeConfig, id string) *ports.SessionDef {
	for i := range cfg.Sessions {
		if cfg.Sessions[i].ID == id {
			return &cfg.Sessions[i]
		}
	}
	return nil
}

func findReceiver(cfg *ports.BridgeConfig, id string) *ports.ReceiverDef {
	for i := range cfg.Receivers {
		if cfg.Receivers[i].ID == id {
			return &cfg.Receivers[i]
		}
	}
	return nil
}

// buildResolver constructs a DestinationResolver from a config ResolverDef.
func buildResolver(def *ports.ResolverDef, bindings []routing.DestinationBinding) (ports.DestinationResolver, error) {
	switch def.Type {
	case "header_map":
		return runtime.NewBindingResolver(bindings,
			runtime.MatchByHeader(def.HeaderKey, def.HeaderMap)), nil

	case "all":
		return runtime.NewBindingResolver(bindings, runtime.MatchAll()), nil

	case "static":
		if len(bindings) > 0 {
			return runtime.NewBindingResolver(bindings,
				runtime.MatchByID(bindings[0].ID)), nil
		}
		return nil, fmt.Errorf("static resolver requires at least one binding")

	case "rules":
		rules := make([]runtime.MatchRule, len(def.Rules))
		for i, r := range def.Rules {
			conds := make([]runtime.MatchCondition, len(r.Match))
			for j, c := range r.Match {
				conds[j] = runtime.MatchCondition{
					Field:    c.Field,
					Operator: c.Operator,
					Value:    c.Value,
				}
			}
			rules[i] = runtime.MatchRule{
				BindingID:  r.BindingID,
				Conditions: conds,
			}
		}

		compiled, err := runtime.CompileMatchRules(rules)
		if err != nil {
			return nil, fmt.Errorf("compile match rules: %w", err)
		}

		return runtime.NewRuleResolver(bindings, compiled, def.DefaultBinding)

	default:
		return nil, fmt.Errorf("unknown resolver type %q", def.Type)
	}
}

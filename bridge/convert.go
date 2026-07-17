package bridge

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

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
		OnFiltered:         routing.FilteredAction(r.Policy.OnFiltered),
		AllowUnfenced:      r.Policy.AllowUnfenced,
		AllowRetryDrop:     r.Policy.AllowRetryDrop,
		TrustBridgeHeaders: r.TrustBridgeHeaders,
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
	if r.Policy.ReplayBudget != "" {
		d, err := time.ParseDuration(r.Policy.ReplayBudget)
		if err != nil {
			return p, fmt.Errorf("invalid replay_budget %q: %w", r.Policy.ReplayBudget, err)
		}
		// time.ParseDuration accepts a leading '-', but a negative budget is
		// nonsensical and WithDefaults would silently coerce it to the 15m
		// default on the real load path (Validate is not called there). Reject
		// it at the parse boundary so the operator sees the misconfiguration
		// (spec §4.2: negative -> validation error).
		if d < 0 {
			return p, fmt.Errorf("invalid replay_budget %q: must not be negative", r.Policy.ReplayBudget)
		}
		p.ReplayBudget = d
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
	if r.Policy.Backoff.Jitter != 0 {
		if r.Policy.Backoff.Jitter < 0 || r.Policy.Backoff.Jitter > 1 {
			return p, fmt.Errorf("invalid backoff jitter %v: must be in [0,1]", r.Policy.Backoff.Jitter)
		}
		p.Backoff.JitterFactor = r.Policy.Backoff.Jitter
	}
	return p, nil
}

// IsClusteredDeployment is the single shared predicate for "is this a clustered
// deployment": deployment_mode is "clustered" OR a static cluster.endpoints
// override is present. A nil config is never clustered.
//
// It is the one definition used across the bridge: the HA-timing baseline for
// lease-bearing sessions (finding HIGH-3, builder/failover/post-acquire callers)
// AND the fail-closed guard that refuses an uncoordinated per-process live
// reload of (or into) a clustered cohort (finding H8, Supervisor.apply and the
// AWS composition root). The config layer mirrors it in
// config.deploymentIsClustered so the two agree on exactly which deployments are
// clustered.
func IsClusteredDeployment(cfg *ports.BridgeConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.Bridge.DeploymentMode == "clustered" {
		return true
	}
	return cfg.Bridge.Cluster != nil && len(cfg.Bridge.Cluster.Endpoints) > 0
}

func toSessionConfigE(rs *ports.RouteSessionDef, clustered bool) (*session.Config, error) {
	if rs == nil {
		return nil, nil
	}

	// A RouteSessionDef source is ALWAYS an exclusive, lease-bearing single-owner
	// session (see the ConnectAfterLease note below). In a CLUSTERED deployment
	// where the operator did NOT pin lease timing (lease_ttl AND renew_interval
	// both empty), start from the lower-latency HA lease cadence (LeaseTTL=45s)
	// rather than the DefaultConfig baseline. This selects lease renewal timing;
	// it is not an end-to-end failure-detection to ServiceLevelFull claim.
	// Explicit operator
	// timing always wins and keeps the DefaultConfig baseline + overrides below
	// (backward-safe). This is also the only live wiring of session.HAConfig.
	useHA := clustered && rs.LeaseTTL == "" && rs.RenewInterval == ""
	var sc session.Config
	if useHA {
		// HAConfig pins RenewInterval=10s/RenewJitter=1s that are internally
		// consistent with its 45s TTL (worst-case renew span 40.5s < 45s); keep
		// them verbatim rather than resetting to derive.
		sc = session.HAConfig(rs.SessionID, true)
	} else {
		sc = session.DefaultConfig(rs.SessionID, true)
		// DefaultConfig pins RenewInterval to a fixed value (110s). Leaving it set
		// suppresses the session manager's documented derive-from-TTL branch
		// (runtime/session/manager.go), so a blueprint that only sets lease_ttl
		// would silently keep the 110s renew cadence regardless of a much shorter
		// TTL. Reset it to zero and only override when the operator explicitly
		// configures renew_interval, letting the session manager derive it from
		// LeaseTTL otherwise (contract C3).
		sc.RenewInterval = 0
		// DefaultConfig also pins RenewJitter (5s). The manager derives jitter only
		// when BOTH RenewInterval and RenewJitter are zero (manager.go: derived
		// only if RenewJitter==0 && renewIntervalDerived); leaving the pinned 5s
		// suppresses derivation, so a lease_ttl-only session gets a fixed 5s jitter
		// instead of the derived renew/4 -- and with a small lease_ttl the
		// expiry-margin clamp then fires on every boot. Reset it to zero for the
		// same reason as RenewInterval, overriding only when lease_renew_jitter is
		// set explicitly (contract C3: the production path leaves both zero).
		sc.RenewJitter = 0
	}
	// F6: default connect_after_lease ON for a RouteSessionDef source. It is
	// always an exclusive single-owner session; deferring connect until the lease
	// is won stops a booting standby from resuming a broker-persisted subscription
	// and consuming without the lease. nil (omitted in the blueprint) => true; an
	// explicit value is honored.
	sc.ConnectAfterLease = rs.ConnectAfterLease == nil || *rs.ConnectAfterLease

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
	if rs.RenewJitter != "" {
		d, err := time.ParseDuration(rs.RenewJitter)
		if err != nil {
			return nil, fmt.Errorf("invalid lease_renew_jitter %q: %w", rs.RenewJitter, err)
		}
		sc.RenewJitter = d
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
	if rs.AcquirePollInterval != "" {
		d, err := time.ParseDuration(rs.AcquirePollInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid acquire_poll_interval %q: %w", rs.AcquirePollInterval, err)
		}
		sc.AcquirePollInterval = d
	}
	// RenewCallTimeout is part of the failover-safety invariant (finding
	// C3-HIGH): renewWorstCaseSpan folds it in, so exposing it lets a deployment
	// tune the safety margin. Zero (unset) keeps the manager's derived default.
	if rs.RenewCallTimeout != "" {
		d, err := time.ParseDuration(rs.RenewCallTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid renew_call_timeout %q: %w", rs.RenewCallTimeout, err)
		}
		sc.RenewCallTimeout = d
	}
	if rs.FailoverSLO != "" {
		d, err := time.ParseDuration(rs.FailoverSLO)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid failover_slo %q: must be a positive duration", rs.FailoverSLO)
		}
		sc.FailoverSLO = d
	}
	if rs.StartupAllowance != "" {
		d, err := time.ParseDuration(rs.StartupAllowance)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("invalid startup_allowance %q: must be a non-negative duration", rs.StartupAllowance)
		}
		sc.StartupAllowance = d
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
func applyBridgeDrainDefaults(sc *session.Config, bs ports.BridgeSettings) {
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
			Transport: transportForBinding(cfg, bd),
			SessionID: bd.SessionID,
			SenderID:  bd.SenderID,
			Address:   bd.Address,
			Config:    bd.Config,
		})
	}
	return out
}

// transportForBinding resolves the transport name a binding belongs to
// by following SenderID → SenderDef.Transport. Falls back to the
// session's transport when the sender does not declare one. Returns ""
// when neither can be resolved (the runtime then treats the binding as
// having no transport-level address validator).
func transportForBinding(cfg *ports.BridgeConfig, bd *ports.BindingDef) string {
	for i := range cfg.Senders {
		if cfg.Senders[i].ID == bd.SenderID {
			if cfg.Senders[i].Transport != "" {
				return cfg.Senders[i].Transport
			}
			if sd := findSession(cfg, cfg.Senders[i].SessionID); sd != nil {
				return sd.Transport
			}
			break
		}
	}
	if bd.SessionID != "" {
		if sd := findSession(cfg, bd.SessionID); sd != nil {
			return sd.Transport
		}
	}
	return ""
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
					Operator: runtime.Operator(c.Operator),
					Value:    runtime.Val(c.Value),
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

// buildAddressValidators returns a binding-ID → AddressValidator map
// populated by querying the per-binding TransportFactory's
// AddressValidator() capability. Bindings whose transport is unknown
// or whose factory returns nil are omitted (the runtime then skips
// validation for those bindings).
func buildAddressValidators(
	transports map[string]ports.TransportFactory,
	bindings []routing.DestinationBinding,
) map[string]ports.AddressValidator {
	out := make(map[string]ports.AddressValidator, len(bindings))
	for _, bd := range bindings {
		if bd.Transport == "" {
			continue
		}
		tf, ok := transports[bd.Transport]
		if !ok {
			continue
		}
		v := tf.AddressValidator()
		if v == nil {
			continue
		}
		out[bd.ID] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

package bridge

import (
	"fmt"
	"math"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func (b *Builder) validateFailoverBudgets() error {
	if b == nil || b.cfg == nil {
		return nil
	}
	for _, route := range b.cfg.Routes {
		if route.Session == nil || route.Session.FailoverSLO == "" {
			continue
		}
		sc, err := toSessionConfigE(route.Session, deploymentClustered(b.cfg))
		if err != nil {
			return fmt.Errorf("bridge: route %q: %w", route.ID, err)
		}
		if err := sc.Validate(); err != nil {
			return fmt.Errorf("bridge: route %q: %w", route.ID, err)
		}
		def := findSession(b.cfg, route.Session.SessionID)
		if def == nil || ports.IsNilPluginConfig(def.Config) {
			return failoverBudgetError(route.ID, route.Session.SessionID, "transport failover timing capability is unavailable")
		}
		capability, ok := def.Config.(ports.TransportFailoverTimingConfig)
		if !ok {
			return failoverBudgetError(route.ID, route.Session.SessionID, "transport failover timing capability is unavailable")
		}
		mode := connectivity.SessionMode(def.SessionMode)
		if mode == "" || sc.Exclusive {
			mode = connectivity.SessionExclusive
		}
		transportTiming := capability.TransportFailoverTiming(mode)
		if transportTiming.PostTakeoverActivation <= 0 {
			return failoverBudgetError(route.ID, route.Session.SessionID, "complete post-takeover activation duration is unknown")
		}
		ttl, acquirePoll, renewCallTimeout := sc.EffectiveFailoverLeaseTiming()
		budget, err := checkedFailoverBudget(ttl, acquirePoll, renewCallTimeout,
			transportTiming.PostTakeoverActivation, sc.StartupAllowance)
		if err != nil {
			return fmt.Errorf("bridge: route %q: session %q: %w", route.ID, route.Session.SessionID, err)
		}
		if budget > sc.FailoverSLO {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bridge: route %q: session %q: failover budget=%s exceeds declared failover_slo=%s",
				route.ID, route.Session.SessionID, budget, sc.FailoverSLO,
			))
		}
	}
	return nil
}

func failoverBudgetError(routeID, sessionID, detail string) error {
	return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
		"bridge: route %q: session %q: declared failover_slo requires known transport timing: %s",
		routeID, sessionID, detail,
	))
}

func checkedFailoverBudget(leaseTTL, acquirePoll, renewCallTimeout, postTakeoverActivation, startupAllowance time.Duration) (time.Duration, error) {
	required := []struct {
		name  string
		value time.Duration
	}{
		{"lease TTL", leaseTTL}, {"acquire poll", acquirePoll}, {"renew call timeout", renewCallTimeout},
		{"complete post-takeover activation", postTakeoverActivation},
	}
	for _, part := range required {
		if part.value <= 0 {
			return 0, shared.ErrInvalidConfig.WithMessage("bridge: failover budget " + part.name + " must be positive")
		}
	}
	if startupAllowance < 0 {
		return 0, shared.ErrInvalidConfig.WithMessage("bridge: failover budget startup allowance must be non-negative")
	}
	pollMargin := acquirePoll / 4
	if acquirePoll%4 != 0 {
		pollMargin++
	}
	if acquirePoll > time.Duration(math.MaxInt64)-pollMargin {
		return 0, shared.ErrInvalidConfig.WithMessage("bridge: failover budget acquire poll margin overflows time.Duration")
	}
	adjustedPoll := acquirePoll + pollMargin
	parts := []time.Duration{leaseTTL, adjustedPoll, renewCallTimeout, postTakeoverActivation, startupAllowance}
	var total time.Duration
	for _, part := range parts {
		if part > time.Duration(math.MaxInt64)-total {
			return 0, shared.ErrInvalidConfig.WithMessage("bridge: failover budget overflows time.Duration")
		}
		total += part
	}
	return total, nil
}

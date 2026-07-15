package bridge

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

type failoverManagerInputs struct {
	leaseTTL               time.Duration
	renewInterval          time.Duration
	renewJitter            time.Duration
	maxRenewFails          int
	acquirePoll            time.Duration
	renewCallTimeout       time.Duration
	stepDownGrace          time.Duration
	connectAfterLease      bool
	failoverSLO            time.Duration
	startupAllowance       time.Duration
	postAcquireActivation  time.Duration
	postTakeoverActivation time.Duration
	transportTimingKnown   bool
	transportKind          string
	mode                   connectivity.SessionMode
}

type failoverManagerCandidate struct {
	sessionID string
	source    string
	config    session.Config
	inputs    failoverManagerInputs
}

func (b *Builder) validateFailoverBudgets() error {
	if b == nil || b.cfg == nil {
		return nil
	}
	candidates, err := b.collectFailoverManagerCandidates()
	if err != nil {
		return err
	}
	bySession := make(map[string][]failoverManagerCandidate)
	for _, candidate := range candidates {
		bySession[candidate.sessionID] = append(bySession[candidate.sessionID], candidate)
	}
	sessionIDs := make([]string, 0, len(bySession))
	for sessionID := range bySession {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		group := bySession[sessionID]
		sort.Slice(group, func(i, j int) bool { return group[i].source < group[j].source })
		canonical := group[0]
		for _, candidate := range group[1:] {
			if candidate.inputs != canonical.inputs {
				return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
					"bridge: session %q has divergent session-manager configuration between %s and %s; shared sessions require identical HA, failover SLO, and transport activation inputs",
					sessionID, canonical.source, candidate.source,
				))
			}
		}
		if canonical.inputs.failoverSLO == 0 {
			continue
		}
		if !canonical.inputs.transportTimingKnown || canonical.inputs.postTakeoverActivation <= 0 {
			return failoverBudgetError(canonical.source, sessionID, "complete post-takeover activation duration is unknown")
		}
		ttl, acquirePoll, renewCallTimeout := canonical.config.EffectiveFailoverLeaseTiming()
		budget, budgetErr := checkedFailoverBudget(ttl, acquirePoll, renewCallTimeout,
			canonical.inputs.postTakeoverActivation, canonical.config.StartupAllowance)
		if budgetErr != nil {
			return fmt.Errorf("bridge: %s: session %q: %w", canonical.source, sessionID, budgetErr)
		}
		if budget > canonical.config.FailoverSLO {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"bridge: %s: session %q: failover budget=%s exceeds declared failover_slo=%s",
				canonical.source, sessionID, budget, canonical.config.FailoverSLO,
			))
		}
	}
	return nil
}

func (b *Builder) collectFailoverManagerCandidates() ([]failoverManagerCandidate, error) {
	var candidates []failoverManagerCandidate
	for _, route := range b.cfg.Routes {
		if route.Session != nil {
			sc, err := toSessionConfigE(route.Session, deploymentClustered(b.cfg))
			if err != nil {
				return nil, fmt.Errorf("bridge: route %q: %w", route.ID, err)
			}
			candidate, err := b.failoverManagerCandidate(fmt.Sprintf("route %q", route.ID), route.Session.SessionID, *sc)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, candidate)
		}
		for _, bindingID := range route.Bindings {
			binding := findBinding(b.cfg, bindingID)
			if binding == nil || binding.SessionID == "" {
				continue
			}
			sc, err := b.bindingSessionConfig(route, binding.SessionID)
			if err != nil {
				return nil, fmt.Errorf("bridge: route %q: binding %q session config: %w", route.ID, bindingID, err)
			}
			sc.ConnectAfterLease = true
			candidate, err := b.failoverManagerCandidate(
				fmt.Sprintf("route %q binding %q", route.ID, bindingID), binding.SessionID, sc,
			)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func (b *Builder) failoverManagerCandidate(source, sessionID string, sc session.Config) (failoverManagerCandidate, error) {
	def := findSession(b.cfg, sessionID)
	if err := configureSessionActivationTiming(source, sessionID, &sc, def); err != nil {
		return failoverManagerCandidate{}, err
	}
	if err := sc.Validate(); err != nil {
		return failoverManagerCandidate{}, fmt.Errorf("bridge: %s: %w", source, err)
	}
	mode := connectivity.SessionExclusive
	transportKind := ""
	activation := time.Duration(0)
	timingKnown := false
	if def != nil {
		transportKind = def.Transport
		if def.SessionMode != "" && !sc.Exclusive {
			mode = connectivity.SessionMode(def.SessionMode)
		}
		if !ports.IsNilPluginConfig(def.Config) {
			if capability, ok := def.Config.(ports.TransportFailoverTimingConfig); ok {
				activation = capability.TransportFailoverTiming(mode).PostTakeoverActivation
				timingKnown = activation > 0
			}
		}
	}
	return failoverManagerCandidate{
		sessionID: sessionID, source: source, config: sc,
		inputs: failoverManagerInputs{
			leaseTTL: sc.LeaseTTL, renewInterval: sc.RenewInterval,
			renewJitter: sc.RenewJitter, maxRenewFails: sc.MaxRenewFails,
			acquirePoll: sc.AcquirePollInterval, renewCallTimeout: sc.RenewCallTimeout,
			stepDownGrace: sc.StepDownGrace, connectAfterLease: sc.ConnectAfterLease,
			failoverSLO: sc.FailoverSLO, startupAllowance: sc.StartupAllowance,
			postAcquireActivation:  sc.PostAcquireActivationTimeout,
			postTakeoverActivation: activation, transportTimingKnown: timingKnown,
			transportKind: transportKind, mode: mode,
		},
	}, nil
}

func failoverBudgetError(source, sessionID, detail string) error {
	return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
		"bridge: %s: session %q: declared failover_slo requires known transport timing: %s",
		source, sessionID, detail,
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

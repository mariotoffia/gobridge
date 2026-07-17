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
			// No SLO declared: nothing to enforce, but the worst case must not
			// stay silent (MQTT-F2). The auto-selected HA profile's 45s lease
			// TTL invites an operator to assume ~45s failover while the real
			// worst case — lease TTL + poll boundaries + acquire-call budget +
			// transport post-takeover activation — is several minutes with
			// default transport timeouts. Disclose the computed budget at
			// every build so the number is on record before an incident, and
			// point at failover_slo, which turns this disclosure into a
			// build-failing contract.
			b.logFailoverBudgetDisclosure(canonical, sessionID)
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
			sc, err := toSessionConfigE(route.Session, IsClusteredDeployment(b.cfg))
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

// logFailoverBudgetDisclosure computes and logs the worst-case failover
// budget for one exclusive session whose config declares NO failover_slo
// (MQTT-F2). Best-effort by design: a budget that cannot be computed
// (unknown transport timing, degenerate lease values) logs what is known
// and never fails the build — enforcement is exactly what declaring
// failover_slo buys.
func (b *Builder) logFailoverBudgetDisclosure(canonical failoverManagerCandidate, sessionID string) {
	if b.logger == nil {
		return
	}
	if !canonical.inputs.transportTimingKnown || canonical.inputs.postTakeoverActivation <= 0 {
		b.logger.Info("bridge: failover budget for exclusive session is UNKNOWN — the transport "+
			"declares no post-takeover activation timing; declare failover_slo to require a computable, "+
			"enforced bound",
			"session_id", sessionID,
			"source", canonical.source,
		)
		return
	}
	ttl, acquirePoll, renewCallTimeout := canonical.config.EffectiveFailoverLeaseTiming()
	budget, err := checkedFailoverBudget(ttl, acquirePoll, renewCallTimeout,
		canonical.inputs.postTakeoverActivation, canonical.config.StartupAllowance)
	if err != nil {
		b.logger.Info("bridge: failover budget for exclusive session could not be computed",
			"session_id", sessionID,
			"source", canonical.source,
			"error", err,
		)
		return
	}
	b.logger.Info("bridge: worst-case failover budget for exclusive session (no failover_slo declared — "+
		"NOT enforced). This is how long a standby may need to fully take over after the active owner "+
		"dies: lease TTL + acquire-poll boundaries + lease-store call budget + transport post-takeover "+
		"activation + startup allowance. Tune lease_ttl/acquire_poll_interval and the transport "+
		"connect/reconcile/grace timeouts to shrink it, and declare failover_slo to make the build FAIL "+
		"when the configuration cannot meet the target",
		"session_id", sessionID,
		"source", canonical.source,
		"failover_budget", budget.String(),
		"lease_ttl", ttl.String(),
		"post_takeover_activation", canonical.inputs.postTakeoverActivation.String(),
		"startup_allowance", canonical.config.StartupAllowance.String(),
	)
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
	minPoll, maxPoll, err := checkedFailoverPollBounds(acquirePoll)
	if err != nil {
		return 0, err
	}
	pollBoundaries, err := checkedDurationProduct(2, maxPoll, "two failover poll boundaries")
	if err != nil {
		return 0, err
	}
	callCount, err := checkedObservationCallCount(leaseTTL, minPoll)
	if err != nil {
		return 0, err
	}
	// Every LeaseStore.Acquire shares one RenewCallTimeout context across its
	// internal Put/Get/observation-CAS/takeover operations. Call latency after a
	// successful observation CAS is deliberately excluded from persisted elapsed,
	// so budget the baseline call plus every possible minimum-jitter observation
	// round. Internal Dynamo operations are not counted separately.
	acquireCallBudget, err := checkedDurationProduct(callCount, renewCallTimeout, "lease observation call timeouts")
	if err != nil {
		return 0, err
	}
	parts := []time.Duration{leaseTTL, pollBoundaries, acquireCallBudget, postTakeoverActivation, startupAllowance}
	var total time.Duration
	for _, part := range parts {
		if part > time.Duration(math.MaxInt64)-total {
			return 0, shared.ErrInvalidConfig.WithMessage("bridge: failover budget overflows time.Duration")
		}
		total += part
	}
	return total, nil
}

func checkedFailoverPollBounds(base time.Duration) (minPoll, maxPoll time.Duration, err error) {
	if base <= 0 {
		return 0, 0, shared.ErrInvalidConfig.WithMessage("bridge: acquire poll must be positive")
	}
	spread := base / 2
	minPoll = base - spread/2
	if minPoll < time.Millisecond {
		minPoll = time.Millisecond
	}
	margin := base / 4
	if base%4 != 0 {
		margin++
	}
	if base > time.Duration(math.MaxInt64)-margin {
		return 0, 0, shared.ErrInvalidConfig.WithMessage("bridge: failover acquire poll maximum overflows time.Duration")
	}
	maxPoll = base + margin
	if maxPoll < time.Millisecond {
		maxPoll = time.Millisecond
	}
	return minPoll, maxPoll, nil
}

func checkedObservationCallCount(ttl, minPoll time.Duration) (int64, error) {
	if ttl <= 0 || minPoll <= 0 {
		return 0, shared.ErrInvalidConfig.WithMessage("bridge: observation call count requires positive TTL and minimum poll")
	}
	rounds := int64(ttl / minPoll)
	if ttl%minPoll != 0 {
		if rounds == math.MaxInt64 {
			return 0, shared.ErrInvalidConfig.WithMessage("bridge: observation round count overflows int64")
		}
		rounds++
	}
	if rounds == math.MaxInt64 {
		return 0, shared.ErrInvalidConfig.WithMessage("bridge: observation call count overflows int64")
	}
	return rounds + 1, nil
}

func checkedDurationProduct(count int64, duration time.Duration, name string) (time.Duration, error) {
	if count <= 0 || duration <= 0 {
		return 0, shared.ErrInvalidConfig.WithMessage("bridge: " + name + " requires positive factors")
	}
	if count > math.MaxInt64/int64(duration) {
		return 0, shared.ErrInvalidConfig.WithMessage("bridge: " + name + " overflow time.Duration")
	}
	return time.Duration(count) * duration, nil
}

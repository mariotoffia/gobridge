package bridge

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// The broker-path failover budget. It is a SECOND failure mode with its own
// timeline, not a variation of owner death, and a declared failover_slo has to
// admit both: an owner that alone loses its broker path keeps renewing, so the
// lease never expires and the whole owner-death formula — which is anchored on
// lease TTL — describes none of what actually happens.
//
// What happens instead, in order:
//
//	broker_health_step_down   the configured non-converged threshold
//	+ one detection round     the due check runs at the top of the renew timer
//	                          case, so a crossing just after a tick waits a full
//	                          round; the loop resets its timer AFTER the body
//	                          returns, so a round is renew_interval + jitter/2 +
//	                          2 x renew_call_timeout — TWO store calls, because
//	                          once a renew streak has reached MaxRenewFails the
//	                          body also re-runs the authoritative Current read on
//	                          every later round, and a node-local fault can
//	                          degrade the store path along with the broker path
//	+ release bound           bounded source close (stop ingress)
//	+ step_down_grace         settlement grace held before the release
//	+ release bound           the bounded lease Release itself
//	+ 2 x poll boundary       a standby whose Acquire is already IN FLIGHT when
//	                          the Release commits loses its observation
//	                          compare-and-set, reads that as ordinary contention,
//	                          and waits a full poll before trying again — the
//	                          same straddle the owner-death formula budgets two
//	                          boundaries for
//	+ 2 x renew_call_timeout  those two Acquire calls. A RELEASED row has an
//	                          empty owner, so the winning one takes over within
//	                          its single call with no observation window to wait
//	                          out; it is the number of ATTEMPTS that is two, not
//	                          the calls inside one
//	+ post-takeover activation
//	+ startup_allowance
//
// Two things are deliberately outside it, on the same footing as the owner-death
// formula's treatment of backend failure. A lease Release that the store REFUSES
// leaves the row owned by an owner that has stopped renewing, so the standby
// waits out the lease TTL and the owner-death path instead — that is a lease
// store failure, and belongs to measured error-budget evidence, not to admission.
// And the transport's OWN detection latency (an MQTT keepalive, say) runs before
// the threshold clock starts, because the clock starts when the transport reports
// the path lost.
type brokerPathBudgetInputs struct {
	brokerHealthStepDown   time.Duration
	renewInterval          time.Duration
	renewJitter            time.Duration
	renewCallTimeout       time.Duration
	acquirePoll            time.Duration
	stepDownGrace          time.Duration
	postTakeoverActivation time.Duration
	startupAllowance       time.Duration
}

// brokerPathBudgetFor projects a resolved session config plus the transport's
// activation bound onto the broker-path inputs, using the same resolution the
// manager runs.
func brokerPathBudgetFor(cfg session.Config, postTakeoverActivation time.Duration) brokerPathBudgetInputs {
	cadence := cfg.EffectiveLeaseCadence()
	grace, _ := cfg.EffectiveStepDownTiming()
	// releaseBound is derived from the resolved grace inside
	// checkedBrokerPathFailoverBudget, so only the grace itself travels here.
	return brokerPathBudgetInputs{
		brokerHealthStepDown:   cfg.BrokerHealthStepDown,
		renewInterval:          cadence.RenewInterval,
		renewJitter:            cadence.RenewJitter,
		renewCallTimeout:       cadence.RenewCallTimeout,
		acquirePoll:            cadence.AcquirePollInterval,
		stepDownGrace:          grace,
		postTakeoverActivation: postTakeoverActivation,
		startupAllowance:       cfg.StartupAllowance,
	}
}

func checkedBrokerPathFailoverBudget(in brokerPathBudgetInputs) (time.Duration, error) {
	required := []struct {
		name  string
		value time.Duration
	}{
		{"broker health step down", in.brokerHealthStepDown},
		{"renew interval", in.renewInterval},
		{"renew call timeout", in.renewCallTimeout},
		{"acquire poll", in.acquirePoll},
		{"complete post-takeover activation", in.postTakeoverActivation},
	}
	for _, part := range required {
		if part.value <= 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				"bridge: broker-path failover budget " + part.name + " must be positive")
		}
	}
	if in.renewJitter < 0 || in.stepDownGrace < 0 || in.startupAllowance < 0 {
		return 0, shared.ErrInvalidConfig.WithMessage(
			"bridge: broker-path failover budget jitter, step-down grace and startup allowance must be non-negative")
	}
	_, maxPoll, err := checkedFailoverPollBounds(in.acquirePoll)
	if err != nil {
		return 0, err
	}
	// The detection round and the two standby Acquire attempts are all bounded by
	// renew_call_timeout, so budget its four occurrences through the same checked
	// multiplication the owner-death formula uses.
	storeCalls, err := checkedDurationProduct(4, in.renewCallTimeout, "broker-path failover store calls")
	if err != nil {
		return 0, err
	}
	pollBoundaries, err := checkedDurationProduct(2, maxPoll, "broker-path failover poll boundaries")
	if err != nil {
		return 0, err
	}
	releaseBound := session.BoundedReleaseTimeout(in.stepDownGrace)
	return checkedDurationSum(
		in.brokerHealthStepDown,
		// The renew cadence of one detection round; its two store calls are in
		// storeCalls above.
		routing.RenewWorstCaseSpan(in.renewInterval, in.renewJitter, 0, 1),
		releaseBound,
		in.stepDownGrace,
		releaseBound,
		pollBoundaries,
		storeCalls,
		in.postTakeoverActivation,
		in.startupAllowance,
	)
}

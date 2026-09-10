package routing

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// The broker-path failover decision. It lives here, in the domain, for the same
// reason the lease cadence does: three boundaries must agree on it — the
// blueprint validator (which must reject before a durable config write), the
// builder that maps a route's session block onto a manager config, and the
// session manager's own validation.

// BrokerPathFailoverOff is the explicit opt-out an operator writes in
// broker_health_step_down. It is a THIRD state, distinct from leaving the field
// empty: empty means the decision was never made, "off" means it was made and
// the answer is no.
const BrokerPathFailoverOff = "off"

// BrokerPathPolicy is the resolved answer to "what should happen when this
// owner alone loses its broker path while the lease store stays reachable?".
//
// Renewals keep succeeding through such an outage, so the lease/owner/process
// failover path never fires and a healthy standby can never take over. Only a
// positive StepDown closes that gap, and enabling it costs a lease churn risk
// when the broker is globally unreachable — which is why the answer must be
// stated rather than inherited from a default.
type BrokerPathPolicy struct {
	// StepDown is how long an active owner may stay non-converged on its broker
	// path before it voluntarily releases the lease. Zero disables it.
	StepDown time.Duration
	// Declared reports that an operator stated the decision, either way.
	Declared bool
}

// Enabled reports whether broker-path failover will actually run.
func (p BrokerPathPolicy) Enabled() bool { return p.StepDown > 0 }

// ParseBrokerPathPolicy maps the wire value of broker_health_step_down onto the
// resolved decision. Empty is undeclared; BrokerPathFailoverOff is an explicit
// disable; anything else must be a positive duration.
func ParseBrokerPathPolicy(raw string) (BrokerPathPolicy, error) {
	switch raw {
	case "":
		return BrokerPathPolicy{}, nil
	case BrokerPathFailoverOff:
		return BrokerPathPolicy{Declared: true}, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return BrokerPathPolicy{}, shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"broker_health_step_down %q must be a positive duration or %q", raw, BrokerPathFailoverOff,
		))
	}
	return BrokerPathPolicy{StepDown: d, Declared: true}, nil
}

// ValidateBrokerPathPolicy rejects a declared failover objective whose
// broker-path decision was never made. A failover_slo is a claim about how long
// a takeover can take; leaving broker-path failover to its default silently
// excludes a whole failure mode from that claim, and the exclusion is invisible
// in the configuration. `subject` names the session in the diagnostic.
func ValidateBrokerPathPolicy(subject string, failoverSLO time.Duration, p BrokerPathPolicy) error {
	if failoverSLO <= 0 || p.Declared {
		return nil
	}
	return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
		"%s: failover_slo is declared but broker_health_step_down is not: a node-local broker outage "+
			"keeps renewing the lease, so no standby can take over and the objective does not cover it. "+
			"Set broker_health_step_down to a positive duration to fail over on it, or to %q to record "+
			"that this deployment accepts an unbounded node-local broker outage",
		subject, BrokerPathFailoverOff,
	))
}

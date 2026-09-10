package validate

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// validateSessionLeaseCadence rejects a route session whose lease cadence, once
// RESOLVED the way the session manager resolves it, either only fits because the
// expiry-margin clamp cut it or lands below a cadence any lease store can serve.
//
// It has to run here, not only in the builder. The admin config transaction
// validates, writes DURABLY, and only then applies; a rule the builder alone
// enforces turns a typo into a durable write plus a failed apply plus a
// rollback, instead of a rejection. That is the same failure the
// broker_health_step_down check above was moved here to close.
//
// The resolution is not re-derived here: it is the domain's
// (routing.LeaseTimingRequest.Resolve), the same code the manager runs. Only the
// blueprint-shaped part is local — parsing the duration strings and applying the
// baseline a route session inherits, which is keyed on the ONE canonical
// clustered predicate (ports.IsClusteredDeployment) the builder also uses.
// bridge's own mapping is pinned against this one by a contract test, so the two
// cannot drift into disagreeing about which configurations are valid.
func validateSessionLeaseCadence(
	ve *ports.BlueprintValidationError,
	prefix string,
	sess *ports.RouteSessionDef,
	clustered bool,
) {
	pinned, ok := pinnedLeaseTiming(sess)
	if !ok {
		// An unparseable duration is already reported by
		// validateSessionDurationFields; resolving a partial request would only
		// add a confusing second error about a value the operator never wrote.
		return
	}
	timing := routing.BaselineLeaseTiming(clustered, pinned).ApplyOverrides(pinned).Resolve()
	if err := timing.ValidateCadence(prefix + ": session"); err != nil {
		ve.Addf("%v", err)
	}
}

// pinnedLeaseTiming reads the lease knobs an operator explicitly set. It reports
// false when any of them is unparseable, leaving the diagnostic to the
// field-level duration check.
func pinnedLeaseTiming(sess *ports.RouteSessionDef) (routing.LeaseTimingRequest, bool) {
	var req routing.LeaseTimingRequest
	for _, f := range []struct {
		val string
		dst *time.Duration
	}{
		{sess.LeaseTTL, &req.LeaseTTL},
		{sess.RenewInterval, &req.RenewInterval},
		{sess.RenewJitter, &req.RenewJitter},
		{sess.RenewCallTimeout, &req.RenewCallTimeout},
		{sess.AcquirePollInterval, &req.AcquirePollInterval},
	} {
		if f.val == "" {
			continue
		}
		d, err := time.ParseDuration(f.val)
		if err != nil {
			return routing.LeaseTimingRequest{}, false
		}
		*f.dst = d
	}
	req.MaxRenewFails = sess.MaxRenewFails
	return req, true
}

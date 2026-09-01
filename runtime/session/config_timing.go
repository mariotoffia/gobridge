package session

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// The lease cadence math lives in domain/routing, because three boundaries must
// agree on it: the blueprint validator (which must reject a bad cadence BEFORE a
// durable config write), the builder that maps a route's session block onto it,
// and this manager. A boundary that re-derives its own subset always drifts from
// the one that runs. This file is the manager's thin projection onto it.

// MinimumLeaseCadence is the documented floor for the RESOLVED renew interval
// and standby acquire poll. See routing.MinimumLeaseCadence.
const MinimumLeaseCadence = routing.MinimumLeaseCadence

// The unexported names below keep the manager's construction path (and its
// tests) reading in this package's own vocabulary.
func deriveRenewInterval(ttl time.Duration, maxRenewFails int) time.Duration {
	return routing.DeriveRenewInterval(ttl, maxRenewFails)
}

func deriveRenewJitter(renewInterval time.Duration) time.Duration {
	return routing.DeriveRenewJitter(renewInterval)
}

func deriveAcquirePollInterval(renewInterval, ttl time.Duration) time.Duration {
	return routing.DeriveAcquirePollInterval(renewInterval, ttl)
}

func deriveRenewCallTimeout(renewInterval time.Duration) time.Duration {
	return routing.DeriveRenewCallTimeout(renewInterval)
}

func renewWorstCaseSpan(renewInterval, renewJitter, renewCallTimeout time.Duration, maxRenewFails int) time.Duration {
	return routing.RenewWorstCaseSpan(renewInterval, renewJitter, renewCallTimeout, maxRenewFails)
}

func clampRenewTimings(
	ttl, renewInterval, renewJitter, renewCallTimeout time.Duration, maxRenewFails int,
) (time.Duration, time.Duration, time.Duration, bool) {
	return routing.ClampRenewTimings(ttl, renewInterval, renewJitter, renewCallTimeout, maxRenewFails)
}

// leaseTimingRequest projects the Config's lease knobs onto the domain request.
// Only these fields participate in cadence resolution; everything else on Config
// (drain, SLO, activation) is outside it.
func (c Config) leaseTimingRequest() routing.LeaseTimingRequest {
	return routing.LeaseTimingRequest{
		LeaseTTL:            c.EffectiveLeaseTTL(),
		RenewInterval:       c.RenewInterval,
		RenewJitter:         c.RenewJitter,
		RenewCallTimeout:    c.RenewCallTimeout,
		AcquirePollInterval: c.AcquirePollInterval,
		MaxRenewFails:       c.MaxRenewFails,
	}
}

// resolveLeaseTiming resolves the cadence exactly as newManager does, so no
// boundary can judge a configuration by values the manager will not run.
func (c Config) resolveLeaseTiming() routing.LeaseTiming {
	return c.leaseTimingRequest().Resolve()
}

// EffectiveLeaseCadence exposes the fully resolved cadence so a boundary that
// resolves the SAME configuration by another route (the blueprint validator,
// which must reject before a durable write and therefore starts from the raw
// route session block) can be pinned against this one.
func (c Config) EffectiveLeaseCadence() routing.LeaseTiming { return c.resolveLeaseTiming() }

// EffectiveFailoverLeaseTiming resolves the three lease-side inputs used by
// failover-budget preflight exactly as Manager construction resolves them.
func (c Config) EffectiveFailoverLeaseTiming() (ttl, acquirePoll, renewCallTimeout time.Duration) {
	t := c.resolveLeaseTiming()
	return t.LeaseTTL, t.AcquirePollInterval, t.RenewCallTimeout
}

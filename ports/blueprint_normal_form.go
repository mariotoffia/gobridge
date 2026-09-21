package ports

import (
	"slices"
	"strings"
	"time"
)

// ContentNormalForm returns a copy of cfg in the ONE shape every content-identity
// check in the project compares (ADR 0016): the Supervisor's no-op reload check,
// the configuration manager's desired-versus-running fingerprint, the AWS
// runtime's skip check and the cluster rollout's candidate and committed-artifact
// digests all decide "is this the same configuration?" over this form, never
// over the bytes a writer happened to produce. Two documents that mean the same
// thing therefore have the same normal form, whichever tool wrote them.
//
// What it rewrites, and why each rewrite keeps the meaning:
//
//   - Version is zeroed. It is the counter writers use to avoid overwriting each
//     other, not something the bridge runs. Ordering of updates is still
//     enforced by the callers that read it (the AWS runtime's stale-source
//     guard), so leaving it out here does not weaken that ordering.
//   - Sessions, Receivers, Senders, Bindings and Routes are sorted by id,
//     stably. Every other part of the document refers to these entries by id,
//     so their position carries no meaning. Every OTHER list keeps its written
//     order: a route's bindings (the first one is the primary session), its
//     processor chain, a resolver's rules, a receiver's subscriptions, the
//     cluster roster, and anything inside a plugin's own options.
//   - Every duration field is rewritten in time.Duration's own spelling, so
//     "30000ms" and "30s" are one value. A value time.ParseDuration cannot read
//     is kept as written: it still takes part in the comparison, so a document
//     that cannot be normalised counts as a change (fail safe).
//   - The two defaults ports itself defines are written out for a value that is
//     LEFT OUT — shutdown_timeout and drain_timeout, the 30 seconds their
//     *Duration accessors fall back to — so a document that omits one and one
//     that writes the default compare equal. A value that is written, even a
//     zero, is kept as written: the validator rejects a zero here, and equating
//     it with the default would let an invalid document pass as a no-op before
//     validation sees it. Defaults owned by other layers (the outbox drainer,
//     the session runtime, a transport) are NOT filled in; an unset value stays
//     unset.
//
// The result is deterministic: the same meaning gives the same value on every
// run and on every node. cfg is never modified. The copy shares the decoded
// plugin Config values by reference and never rewrites them. A nil cfg gives
// nil.
func ContentNormalForm(cfg *BridgeConfig) *BridgeConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.Version = 0
	out.Bridge = normalBridgeSettings(cfg.Bridge)
	if cfg.ConfigWatch != nil {
		cw := *cfg.ConfigWatch
		cw.PollInterval = canonicalDuration(cw.PollInterval, 0)
		cw.Debounce = canonicalDuration(cw.Debounce, 0)
		out.ConfigWatch = &cw
	}
	out.Sessions = sortedByID(cfg.Sessions, func(d SessionDef) string { return d.ID })
	out.Receivers = sortedByID(cfg.Receivers, func(d ReceiverDef) string { return d.ID })
	out.Senders = sortedByID(cfg.Senders, func(d SenderDef) string { return d.ID })
	out.Bindings = sortedByID(cfg.Bindings, func(d BindingDef) string { return d.ID })
	out.Routes = sortedByID(cfg.Routes, func(d RouteDef) string { return d.ID })
	for i := range out.Routes {
		out.Routes[i] = normalRoute(out.Routes[i])
	}
	return &out
}

// sortedByID returns a stably sorted copy of in, keyed by id, or nil when there
// is nothing to sort so an absent list and an empty one are the same list.
// A stable sort keeps duplicate ids in their written order, so the result is
// still deterministic for a document the validator would reject.
func sortedByID[T any](in []T, id func(T) string) []T {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(a, b T) int { return strings.Compare(id(a), id(b)) })
	return out
}

func normalBridgeSettings(b BridgeSettings) BridgeSettings {
	b.ShutdownTimeout = canonicalDuration(b.ShutdownTimeout, defaultBridgeTimeout)
	b.DrainTimeout = canonicalDuration(b.DrainTimeout, defaultBridgeTimeout)
	b.PerRecordDrainTimeout = canonicalDuration(b.PerRecordDrainTimeout, 0)
	b.MaxDrainTimeout = canonicalDuration(b.MaxDrainTimeout, 0)
	if b.Cluster != nil {
		c := *b.Cluster
		// ConfirmWindowDuration treats an empty, zero or negative window alike,
		// but the validator accepts only an empty or positive one, so a written
		// zero stays distinct from an omitted window here (see canonicalDuration).
		c.ConfirmWindow = canonicalDuration(c.ConfirmWindow, 0)
		b.Cluster = &c
	}
	return b
}

func normalRoute(r RouteDef) RouteDef {
	r.Policy.AckAfter = canonicalDuration(r.Policy.AckAfter, 0)
	r.Policy.ReplayBudget = canonicalDuration(r.Policy.ReplayBudget, 0)
	r.Policy.SendTimeout = canonicalDuration(r.Policy.SendTimeout, 0)
	r.Policy.DepthCacheTTL = canonicalDuration(r.Policy.DepthCacheTTL, 0)
	r.Policy.Backoff.InitialInterval = canonicalDuration(r.Policy.Backoff.InitialInterval, 0)
	r.Policy.Backoff.MaxInterval = canonicalDuration(r.Policy.Backoff.MaxInterval, 0)
	if r.Session != nil {
		s := *r.Session
		s.LeaseTTL = canonicalDuration(s.LeaseTTL, 0)
		s.RenewInterval = canonicalDuration(s.RenewInterval, 0)
		s.RenewJitter = canonicalDuration(s.RenewJitter, 0)
		s.StepDownGrace = canonicalDuration(s.StepDownGrace, 0)
		s.AcquirePollInterval = canonicalDuration(s.AcquirePollInterval, 0)
		s.RenewCallTimeout = canonicalDuration(s.RenewCallTimeout, 0)
		s.FailoverSLO = canonicalDuration(s.FailoverSLO, 0)
		s.StartupAllowance = canonicalDuration(s.StartupAllowance, 0)
		// "off" is a keyword of its own here, and canonicalDuration keeps it.
		s.BrokerHealthStepDown = canonicalDuration(s.BrokerHealthStepDown, 0)
		s.DrainInterval = canonicalDuration(s.DrainInterval, 0)
		if s.DrainStrategy != nil {
			ds := *s.DrainStrategy
			ds.Interval = canonicalDuration(ds.Interval, 0)
			ds.MinInterval = canonicalDuration(ds.MinInterval, 0)
			ds.MaxInterval = canonicalDuration(ds.MaxInterval, 0)
			s.DrainStrategy = &ds
		}
		r.Session = &s
	}
	return r
}

// canonicalDuration rewrites raw in time.Duration's own spelling. dflt is the
// value an OMITTED field resolves to when ports owns that default; a zero dflt
// means no default is filled in here and an omitted value stays omitted.
//
// Only an omitted value receives the default. A written value is kept as
// written, an explicit zero included: the validator rejects a zero timeout, and
// equating it with the default would let an invalid document be adopted as a
// no-op before validation sees it. A string time.ParseDuration cannot read is
// also returned as written, so it still counts as a change.
func canonicalDuration(raw string, dflt time.Duration) string {
	if raw == "" {
		if dflt > 0 {
			return dflt.String()
		}
		return ""
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return raw
	}
	return d.String()
}

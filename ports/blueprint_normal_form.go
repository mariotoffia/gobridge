package ports

import (
	"fmt"
	"reflect"
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
//     so their position carries no meaning. The cluster roster is sorted as
//     well, because the bridge reads it as a set. Every OTHER list keeps its
//     written order: a route's bindings (the first one is the primary
//     session), its processor chain, a resolver's rules, a receiver's
//     subscriptions, and anything inside a plugin's own options.
//   - Every duration field is rewritten in time.Duration's own spelling, so
//     "30000ms" and "30s" are one value. A value time.ParseDuration cannot read
//     is kept as written: it still takes part in the comparison, so a document
//     that cannot be normalised counts as a change (fail safe).
//   - A resolver rule's condition value is written the way the runtime coerces
//     it (see normalConditionValue), so two rules that match differently at
//     runtime — an empty map, an empty list, no value at all — never share an
//     identity, whatever a projection later does with empty collections.
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
		// The roster is a set everywhere the bridge reads it (deployment
		// admission, the reload preflight and the rollout coordinator all sort
		// it), so a reordered roster is the same cohort.
		if len(c.Members) > 0 {
			c.Members = slices.Sorted(slices.Values(c.Members))
		}
		// ConfirmWindowDuration treats an empty, zero or negative window alike,
		// but the validator accepts only an empty or positive one, so a written
		// zero stays distinct from an omitted window here (see canonicalDuration).
		c.ConfirmWindow = canonicalDuration(c.ConfirmWindow, 0)
		b.Cluster = &c
	}
	return b
}

func normalRoute(r RouteDef) RouteDef {
	// Policy.AckAfter is a keyword (an acknowledgement boundary such as
	// "outbox_persist"), not a duration, so it is compared as written.
	r.Policy.ReplayBudget = canonicalDuration(r.Policy.ReplayBudget, 0)
	// An explicit zero turns in-process send retry off while an omitted budget
	// takes the default, so no default is filled in and the two stay distinct.
	r.Policy.SendRetryBudget = canonicalDuration(r.Policy.SendRetryBudget, 0)
	r.Policy.SendTimeout = canonicalDuration(r.Policy.SendTimeout, 0)
	r.Policy.DepthCacheTTL = canonicalDuration(r.Policy.DepthCacheTTL, 0)
	r.Policy.Backoff.InitialInterval = canonicalDuration(r.Policy.Backoff.InitialInterval, 0)
	r.Policy.Backoff.MaxInterval = canonicalDuration(r.Policy.Backoff.MaxInterval, 0)
	if r.Resolver != nil {
		res := *r.Resolver
		res.Rules = normalRules(res.Rules)
		r.Resolver = &res
	}
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

// normalRules copies the rules, in their written order, with every condition
// value written the way the runtime coerces it. The rules and their match
// lists are cloned so the caller's config is never modified.
func normalRules(rules []RuleDef) []RuleDef {
	if len(rules) == 0 {
		return rules
	}
	out := slices.Clone(rules)
	for i := range out {
		if len(out[i].Match) == 0 {
			continue
		}
		match := slices.Clone(out[i].Match)
		for j := range match {
			match[j].Value = normalConditionValue(match[j].Value)
		}
		out[i].Match = match
	}
	return out
}

// emptyConditionList is the value ContentNormalForm writes for a resolver rule
// whose condition value is an empty list. At runtime an empty list matches
// nothing, an absent value matches null and a literal string "[]" matches a
// field with that text, so the three must stay three different rules in the
// identity. A projection that reduces empty collections would not tell an
// empty list from an absent value, and a string could be written in a
// document, so the marker is a typed value no document can carry. It is
// unexported so no caller can place it in a condition value either, where it
// would collide with a real empty list.
type emptyConditionList struct {
	EmptyList bool `json:"empty_list"`
}

// normalConditionValue writes a rule's condition value the way the runtime's
// coercion (runtime.Val) reads it, so the identity of a rule follows how the
// rule behaves:
//
//   - nil stays nil: it is the kind that matches a null field;
//   - a string and a bool keep their kind;
//   - every number, including a decoded JSON number, becomes the float64 the
//     runtime compares with, so two integers the runtime cannot tell apart are
//     one rule here as well; a negative zero becomes the plain zero, because
//     the runtime compares the two as equal while a JSON projection would
//     write "-0" and "0";
//   - the lists the runtime knows — []any, []string, []float64 and []int —
//     become lists of the same normalised elements; an empty one becomes
//     the private empty-list marker;
//   - anything else — a map, a struct, any other slice — becomes the string
//     fmt.Sprint gives, which is exactly what the runtime compares for such a
//     value.
//
// fmt.Sprint writes map keys in sorted order, so the result is deterministic.
func normalConditionValue(v any) any {
	if v == nil {
		return nil
	}
	// A value this function wrote on an earlier pass is already in its final
	// form; leaving it alone is what makes ContentNormalForm idempotent.
	if _, ok := v.(emptyConditionList); ok {
		return v
	}
	// The switch names the exact concrete types the runtime special-cases. A
	// named type — even one whose underlying type is a number, or one that
	// happens to have a Float64 method — is not one of them, and the runtime
	// stringifies it, so it falls through to fmt.Sprint below.
	switch val := v.(type) {
	case string, bool:
		return v
	case float64:
		return plainFloat(val)
	case float32:
		return plainFloat(float64(val))
	case int:
		return plainFloat(float64(val))
	case int8:
		return plainFloat(float64(val))
	case int16:
		return plainFloat(float64(val))
	case int32:
		return plainFloat(float64(val))
	case int64:
		return plainFloat(float64(val))
	case uint:
		return plainFloat(float64(val))
	case uint8:
		return plainFloat(float64(val))
	case uint16:
		return plainFloat(float64(val))
	case uint32:
		return plainFloat(float64(val))
	case uint64:
		return plainFloat(float64(val))
	case []any:
		return normalConditionList(len(val), func(i int) any { return val[i] })
	case []string:
		return normalConditionList(len(val), func(i int) any { return val[i] })
	case []float64:
		return normalConditionList(len(val), func(i int) any { return val[i] })
	case []int:
		return normalConditionList(len(val), func(i int) any { return val[i] })
	}
	// A decoded JSON number is the float64 the runtime compares with. It is
	// matched by its exact type, named without importing encoding/json so this
	// package carries no JSON dependency; when it cannot be parsed the runtime
	// keeps its text, which is what fmt.Sprint gives below.
	if t := reflect.TypeOf(v); t.PkgPath() == "encoding/json" && t.Name() == "Number" {
		if n, ok := v.(interface{ Float64() (float64, error) }); ok {
			if f, err := n.Float64(); err == nil {
				return plainFloat(f)
			}
		}
	}
	return fmt.Sprint(v)
}

// plainFloat returns f with a negative zero written as the plain zero.
func plainFloat(f float64) float64 {
	if f == 0 {
		return 0
	}
	return f
}

// normalConditionList writes the n elements at(i) as a list of normalised
// elements, or the private empty-list marker when there are none.
func normalConditionList(n int, at func(int) any) any {
	if n == 0 {
		return emptyConditionList{EmptyList: true}
	}
	out := make([]any, n)
	for i := range out {
		out[i] = normalConditionValue(at(i))
	}
	return out
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

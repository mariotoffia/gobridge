package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// MatchFunc determines whether a binding should be selected for a given envelope.
type MatchFunc func(env *messaging.Envelope, b routing.DestinationBinding) bool

// BindingResolver is a production DestinationResolver that selects bindings
// using a MatchFunc and renders address templates from envelope headers.
// Address validation is no longer the resolver's responsibility — the
// route runner invokes the per-transport AddressValidator (supplied via
// TransportFactory.AddressValidator) against each rendered DispatchPlan
// before dispatch, so resolvers stay transport-agnostic.
type BindingResolver struct {
	bindings []routing.DestinationBinding
	matchFn  MatchFunc
}

// NewBindingResolver creates a resolver that evaluates matchFn against each
// configured binding. Bindings whose Address contains {key} placeholders are
// rendered using envelope headers.
func NewBindingResolver(bindings []routing.DestinationBinding, matchFn MatchFunc) *BindingResolver {
	return &BindingResolver{
		bindings: bindings,
		matchFn:  matchFn,
	}
}

// Resolve selects matching bindings, renders address templates, and returns
// one DispatchPlan per match. Returns a Rejected error when no binding matches.
func (r *BindingResolver) Resolve(_ context.Context, env *messaging.Envelope) ([]routing.DispatchPlan, error) {
	var plans []routing.DispatchPlan

	for _, b := range r.bindings {
		if !r.matchFn(env, b) {
			continue
		}

		addr, err := route.RenderAddress(b.Address, env.Headers())
		if err != nil {
			return nil, shared.ErrInvalidTopic.
				WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
		}

		plans = append(plans, routing.DispatchPlan{
			BindingID: b.ID,
			Address:   addr,
			Headers:   route.CopyHeaders(b.Headers),
		})
	}

	if len(plans) == 0 {
		return nil, shared.NewBridgeError(
			shared.ErrCodeNoBindingMatch, shared.ErrorRejected,
			"no binding matched the envelope",
		)
	}

	return plans, nil
}

// StaticResolver always returns the same pre-configured dispatch plans.
// Use for routes with a fixed single binding or a constant fan-out set.
type StaticResolver struct {
	plans []routing.DispatchPlan
}

// NewStaticResolver creates a resolver that returns plans on every call.
func NewStaticResolver(plans ...routing.DispatchPlan) *StaticResolver {
	cp := make([]routing.DispatchPlan, len(plans))
	copy(cp, plans)
	return &StaticResolver{plans: cp}
}

// Resolve returns a copy of the pre-configured plans to prevent callers
// from mutating the resolver's internal state.
func (r *StaticResolver) Resolve(_ context.Context, _ *messaging.Envelope) ([]routing.DispatchPlan, error) {
	cp := make([]routing.DispatchPlan, len(r.plans))
	copy(cp, r.plans)
	return cp, nil
}

// PlanCount reports how many dispatch plans this resolver always yields. It
// lets pre-start validation statically reject a StaticResolver that would emit
// more than one plan on a route that can only dispatch a single leg
// (direct_hold / DispatchSingle), where the extra plans would otherwise be
// silently discarded at runtime (finding 4).
func (r *StaticResolver) PlanCount() int { return len(r.plans) }

// MatchByHeader returns a MatchFunc that selects a binding when the envelope
// header identified by headerKey has a value that maps to the binding's ID
// in bindingMap. This implements the SQS-to-factory pattern: header "factory"
// value "A" maps to binding ID "mqtt-factory-a-orders".
func MatchByHeader(headerKey string, bindingMap map[string]string) MatchFunc {
	return func(env *messaging.Envelope, b routing.DestinationBinding) bool {
		val, ok := messaging.GetHeaderString(env.Headers(), headerKey)
		if !ok {
			return false
		}
		targetID, exists := bindingMap[val]
		return exists && targetID == b.ID
	}
}

// MatchAll returns a MatchFunc that selects every binding (static fan-out).
func MatchAll() MatchFunc {
	return func(_ *messaging.Envelope, _ routing.DestinationBinding) bool {
		return true
	}
}

// MatchByID returns a MatchFunc that selects only the binding with the given ID.
func MatchByID(bindingID string) MatchFunc {
	return func(_ *messaging.Envelope, b routing.DestinationBinding) bool {
		return b.ID == bindingID
	}
}

// MatchBySubjectPrefix returns a MatchFunc that selects a binding when the
// envelope's Subject starts with a prefix that maps to the binding's ID.
// The prefixMap keys are subject prefixes, values are binding IDs.
func MatchBySubjectPrefix(prefixMap map[string]string) MatchFunc {
	return func(env *messaging.Envelope, b routing.DestinationBinding) bool {
		for prefix, targetID := range prefixMap {
			if strings.HasPrefix(env.Subject(), prefix) && targetID == b.ID {
				return true
			}
		}
		return false
	}
}

// MatchRule pairs a binding ID with conditions that must all match.
type MatchRule struct {
	BindingID  string
	Conditions []MatchCondition
	evals      []*conditionEval
}

// CompileMatchRules pre-compiles all conditions in the rule set. Call this
// once at startup; the returned rules contain cached regex patterns and are
// safe for concurrent use. Returns an error if any condition fails to compile
// (e.g., invalid regex pattern).
func CompileMatchRules(rules []MatchRule) ([]MatchRule, error) {
	compiled := make([]MatchRule, len(rules))
	for i, r := range rules {
		evals := make([]*conditionEval, len(r.Conditions))
		for j, c := range r.Conditions {
			eval, err := newConditionEval(c)
			if err != nil {
				return nil, fmt.Errorf("rule %d (binding %q), condition %d: %w",
					i, r.BindingID, j, err)
			}
			evals[j] = eval
		}
		compiled[i] = MatchRule{
			BindingID:  r.BindingID,
			Conditions: r.Conditions,
			evals:      evals,
		}
	}
	return compiled, nil
}

// RuleResolver implements ports.DestinationResolver with ordered rule
// evaluation (first-match-wins). Rules are evaluated in order; the first
// rule whose conditions all match determines the target binding.
// If no rule matches and a default binding is set, it is used as fallback.
//
// Unlike MatchFunc-based resolvers, RuleResolver guarantees that exactly
// one binding is selected per envelope, providing true first-match semantics.
type RuleResolver struct {
	bindings       []routing.DestinationBinding
	bindingIndex   map[string]routing.DestinationBinding
	rules          []MatchRule
	defaultBinding string
}

// NewRuleResolver creates a rule-based resolver. Rules must be pre-compiled
// via CompileMatchRules. Returns an error if a rule references a binding
// not present in the bindings list.
func NewRuleResolver(
	bindings []routing.DestinationBinding,
	rules []MatchRule,
	defaultBinding string,
) (*RuleResolver, error) {
	idx := make(map[string]routing.DestinationBinding, len(bindings))
	for _, b := range bindings {
		idx[b.ID] = b
	}

	for i, r := range rules {
		if _, ok := idx[r.BindingID]; !ok {
			return nil, fmt.Errorf("rule %d references unknown binding %q", i, r.BindingID)
		}
	}
	if defaultBinding != "" {
		if _, ok := idx[defaultBinding]; !ok {
			return nil, fmt.Errorf("default binding %q not found in bindings", defaultBinding)
		}
	}

	return &RuleResolver{
		bindings:       bindings,
		bindingIndex:   idx,
		rules:          rules,
		defaultBinding: defaultBinding,
	}, nil
}

// Resolve evaluates rules in order and returns a single dispatch plan for
// the first matching rule. Condition evaluation errors are treated as
// non-matching; if all rules fail with errors, the message goes to the
// default binding or returns ErrNoBindingMatch.
func (r *RuleResolver) Resolve(_ context.Context, env *messaging.Envelope) ([]routing.DispatchPlan, error) {
	ctx := newEvalContext()

	for _, rule := range r.rules {
		if len(rule.evals) == 0 {
			return r.planForBinding(rule.BindingID, env)
		}

		allMatch := true
		for _, eval := range rule.evals {
			ok, err := eval.evaluate(env, ctx)
			if err != nil || !ok {
				allMatch = false
				break
			}
		}
		if allMatch {
			return r.planForBinding(rule.BindingID, env)
		}
	}

	if r.defaultBinding != "" {
		return r.planForBinding(r.defaultBinding, env)
	}

	return nil, shared.NewBridgeError(
		shared.ErrCodeNoBindingMatch, shared.ErrorRejected,
		"no rule matched the envelope",
	)
}

func (r *RuleResolver) planForBinding(bindingID string, env *messaging.Envelope) ([]routing.DispatchPlan, error) {
	b := r.bindingIndex[bindingID]

	addr, err := route.RenderAddress(b.Address, env.Headers())
	if err != nil {
		return nil, shared.ErrInvalidTopic.
			WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
	}

	return []routing.DispatchPlan{{
		BindingID: b.ID,
		Address:   addr,
		Headers:   route.CopyHeaders(b.Headers),
	}}, nil
}

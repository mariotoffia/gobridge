package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain"
)

// MatchFunc determines whether a binding should be selected for a given envelope.
type MatchFunc func(env *domain.Envelope, b domain.DestinationBinding) bool

// BindingResolver is a production DestinationResolver that selects bindings
// using a MatchFunc, renders address templates from envelope headers, and
// validates MQTT topics when the binding transport is "mqtt".
type BindingResolver struct {
	bindings []domain.DestinationBinding
	matchFn  MatchFunc
}

// NewBindingResolver creates a resolver that evaluates matchFn against each
// configured binding. Bindings whose Address contains {key} placeholders are
// rendered using envelope headers. MQTT bindings have their rendered addresses
// validated for wildcard safety.
func NewBindingResolver(bindings []domain.DestinationBinding, matchFn MatchFunc) *BindingResolver {
	return &BindingResolver{
		bindings: bindings,
		matchFn:  matchFn,
	}
}

// Resolve selects matching bindings, renders address templates, and returns
// one DispatchPlan per match. Returns a Rejected error when no binding matches.
func (r *BindingResolver) Resolve(_ context.Context, env *domain.Envelope) ([]domain.DispatchPlan, error) {
	var plans []domain.DispatchPlan

	for _, b := range r.bindings {
		if !r.matchFn(env, b) {
			continue
		}

		addr, err := RenderAddress(b.Address, env.Headers)
		if err != nil {
			return nil, domain.ErrInvalidTopic.
				WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
		}

		if strings.EqualFold(b.Transport, "mqtt") {
			if err := ValidateMQTTTopic(addr); err != nil {
				return nil, domain.ErrInvalidTopic.
					WithMessage(fmt.Sprintf("binding %q: %v", b.ID, err))
			}
		}

		plans = append(plans, domain.DispatchPlan{
			BindingID: b.ID,
			Address:   addr,
			Headers:   copyHeaders(b.Options),
		})
	}

	if len(plans) == 0 {
		return nil, domain.NewBridgeError(
			domain.ErrCodeNoBindingMatch, domain.ErrorRejected,
			"no binding matched the envelope",
		)
	}

	return plans, nil
}

// StaticResolver always returns the same pre-configured dispatch plans.
// Use for routes with a fixed single binding or a constant fan-out set.
type StaticResolver struct {
	plans []domain.DispatchPlan
}

// NewStaticResolver creates a resolver that returns plans on every call.
func NewStaticResolver(plans ...domain.DispatchPlan) *StaticResolver {
	cp := make([]domain.DispatchPlan, len(plans))
	copy(cp, plans)
	return &StaticResolver{plans: cp}
}

// Resolve returns a copy of the pre-configured plans to prevent callers
// from mutating the resolver's internal state.
func (r *StaticResolver) Resolve(_ context.Context, _ *domain.Envelope) ([]domain.DispatchPlan, error) {
	cp := make([]domain.DispatchPlan, len(r.plans))
	copy(cp, r.plans)
	return cp, nil
}

// MatchByHeader returns a MatchFunc that selects a binding when the envelope
// header identified by headerKey has a value that maps to the binding's ID
// in bindingMap. This implements the SQS-to-factory pattern: header "factory"
// value "A" maps to binding ID "mqtt-factory-a-orders".
func MatchByHeader(headerKey string, bindingMap map[string]string) MatchFunc {
	return func(env *domain.Envelope, b domain.DestinationBinding) bool {
		val, ok := domain.GetHeaderString(env.Headers, headerKey)
		if !ok {
			return false
		}
		targetID, exists := bindingMap[val]
		return exists && targetID == b.ID
	}
}

// MatchAll returns a MatchFunc that selects every binding (static fan-out).
func MatchAll() MatchFunc {
	return func(_ *domain.Envelope, _ domain.DestinationBinding) bool {
		return true
	}
}

// MatchByID returns a MatchFunc that selects only the binding with the given ID.
func MatchByID(bindingID string) MatchFunc {
	return func(_ *domain.Envelope, b domain.DestinationBinding) bool {
		return b.ID == bindingID
	}
}

// MatchBySubjectPrefix returns a MatchFunc that selects a binding when the
// envelope's Subject starts with a prefix that maps to the binding's ID.
// The prefixMap keys are subject prefixes, values are binding IDs.
func MatchBySubjectPrefix(prefixMap map[string]string) MatchFunc {
	return func(env *domain.Envelope, b domain.DestinationBinding) bool {
		for prefix, targetID := range prefixMap {
			if strings.HasPrefix(env.Subject, prefix) && targetID == b.ID {
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
	bindings       []domain.DestinationBinding
	bindingIndex   map[string]domain.DestinationBinding
	rules          []MatchRule
	defaultBinding string
}

// NewRuleResolver creates a rule-based resolver. Rules must be pre-compiled
// via CompileMatchRules. Returns an error if a rule references a binding
// not present in the bindings list.
func NewRuleResolver(
	bindings []domain.DestinationBinding,
	rules []MatchRule,
	defaultBinding string,
) (*RuleResolver, error) {
	idx := make(map[string]domain.DestinationBinding, len(bindings))
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
func (r *RuleResolver) Resolve(_ context.Context, env *domain.Envelope) ([]domain.DispatchPlan, error) {
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

	return nil, domain.NewBridgeError(
		domain.ErrCodeNoBindingMatch, domain.ErrorRejected,
		"no rule matched the envelope",
	)
}

func (r *RuleResolver) planForBinding(bindingID string, env *domain.Envelope) ([]domain.DispatchPlan, error) {
	b := r.bindingIndex[bindingID]

	addr, err := RenderAddress(b.Address, env.Headers)
	if err != nil {
		return nil, domain.ErrInvalidTopic.
			WithMessage(fmt.Sprintf("binding %q: address template error: %v", b.ID, err))
	}

	if strings.EqualFold(b.Transport, "mqtt") {
		if err := ValidateMQTTTopic(addr); err != nil {
			return nil, domain.ErrInvalidTopic.
				WithMessage(fmt.Sprintf("binding %q: %v", b.ID, err))
		}
	}

	return []domain.DispatchPlan{{
		BindingID: b.ID,
		Address:   addr,
		Headers:   copyHeaders(b.Options),
	}}, nil
}

// RenderAddress replaces {key} placeholders in template with values from vars.
// Returns an error if a placeholder references a missing key or if the rendered
// result is empty. Substituted values are never re-expanded, preventing
// infinite loops and header-value injection.
func RenderAddress(template string, vars map[string]any) (string, error) {
	if template == "" {
		return "", nil
	}

	var b strings.Builder
	remaining := template
	for remaining != "" {
		start := strings.Index(remaining, "{")
		if start < 0 {
			b.WriteString(remaining)
			break
		}
		end := strings.Index(remaining[start:], "}")
		if end < 0 {
			b.WriteString(remaining)
			break
		}
		end += start

		key := remaining[start+1 : end]
		if key == "" {
			return "", fmt.Errorf("empty placeholder in address template %q", template)
		}

		val, ok := domain.GetHeaderString(vars, key)
		if !ok {
			return "", fmt.Errorf("address template placeholder {%s} not found in headers", key)
		}

		b.WriteString(remaining[:start])
		b.WriteString(val)
		remaining = remaining[end+1:]
	}

	result := b.String()
	if result == "" {
		return "", fmt.Errorf("address template %q rendered to empty string", template)
	}

	return result, nil
}

const maxMQTTTopicLen = 65535

// ValidateMQTTTopic rejects MQTT wildcard characters, empty segments, null
// bytes, reserved $-prefixed topics, and topics exceeding the spec maximum
// length in a rendered topic string. Call this on resolved addresses before
// publishing to MQTT.
func ValidateMQTTTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("MQTT topic must not be empty")
	}
	if len(topic) > maxMQTTTopicLen {
		return fmt.Errorf("MQTT topic exceeds maximum length of %d bytes", maxMQTTTopicLen)
	}
	if strings.HasPrefix(topic, "$") {
		return fmt.Errorf("MQTT publish topic must not start with '$' (reserved)")
	}
	if strings.ContainsRune(topic, '+') {
		return fmt.Errorf("MQTT publish topic must not contain wildcard '+'")
	}
	if strings.ContainsRune(topic, '#') {
		return fmt.Errorf("MQTT publish topic must not contain wildcard '#'")
	}
	if strings.ContainsRune(topic, 0) {
		return fmt.Errorf("MQTT topic must not contain null character")
	}
	segments := strings.Split(topic, "/")
	for _, seg := range segments {
		if seg == "" {
			return fmt.Errorf("MQTT topic must not contain empty segments")
		}
	}
	return nil
}

func copyHeaders(opts map[string]any) map[string]any {
	if len(opts) == 0 {
		return nil
	}
	cp := make(map[string]any, len(opts))
	for k, v := range opts {
		cp[k] = deepCopyHeaderValue(v)
	}
	return cp
}

func deepCopyHeaderValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		cp := make(map[string]any, len(val))
		for k, v := range val {
			cp[k] = deepCopyHeaderValue(v)
		}
		return cp
	case []any:
		s := make([]any, len(val))
		for i, elem := range val {
			s[i] = deepCopyHeaderValue(elem)
		}
		return s
	case []string:
		s := make([]string, len(val))
		copy(s, val)
		return s
	case []byte:
		if val == nil {
			return val
		}
		s := make([]byte, len(val))
		copy(s, val)
		return s
	default:
		return v
	}
}

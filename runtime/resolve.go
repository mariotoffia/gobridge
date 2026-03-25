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
			"NO_BINDING_MATCH", domain.ErrorRejected,
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

// Resolve returns the pre-configured plans.
func (r *StaticResolver) Resolve(_ context.Context, _ *domain.Envelope) ([]domain.DispatchPlan, error) {
	return r.plans, nil
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

// RenderAddress replaces {key} placeholders in template with values from vars.
// Returns an error if a placeholder references a missing key or if the rendered
// result is empty.
func RenderAddress(template string, vars map[string]any) (string, error) {
	if template == "" {
		return "", nil
	}

	result := template
	for {
		start := strings.Index(result, "{")
		if start < 0 {
			break
		}
		end := strings.Index(result[start:], "}")
		if end < 0 {
			break
		}
		end += start

		key := result[start+1 : end]
		if key == "" {
			return "", fmt.Errorf("empty placeholder in address template %q", template)
		}

		val, ok := domain.GetHeaderString(vars, key)
		if !ok {
			return "", fmt.Errorf("address template placeholder {%s} not found in headers", key)
		}

		result = result[:start] + val + result[end+1:]
	}

	if result == "" {
		return "", fmt.Errorf("address template %q rendered to empty string", template)
	}

	return result, nil
}

// ValidateMQTTTopic rejects MQTT wildcard characters, empty segments, and null
// bytes in a rendered topic string. Call this on resolved addresses before
// publishing to MQTT.
func ValidateMQTTTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("MQTT topic must not be empty")
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
		cp[k] = v
	}
	return cp
}

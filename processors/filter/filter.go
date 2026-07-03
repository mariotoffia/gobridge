package filter

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ErrRouteRequired signals that an ActionRoute filter was constructed
// without a RouteTo target. It is a setup-time error classified as a
// permanent invalid-payload condition so callers can match via
// errors.Is(err, ErrRouteRequired) consistently with per-message
// errors emitted elsewhere in the runtime.
var ErrRouteRequired = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "route action requires routeTo configuration",
}

// ErrUnknownAction signals that a filter was constructed with an Action
// that is not one of ActionPass / ActionDrop / ActionRoute. Validated in
// New so a misconfigured filter fails fast at build rather than silently
// falling through to the chain (which would turn a policy gate into a
// no-op). Same setup-error convention as ErrRouteRequired.
var ErrUnknownAction = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "filter: unknown action",
}

// ErrUnknownOperator signals that a condition was constructed with an
// unsupported Operator. Validated in New (via newConditionEvaluator) so
// the misconfiguration fails the build instead of degrading into a
// per-message plain error the runtime would treat as retryable.
var ErrUnknownOperator = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "filter: unknown operator",
}

// ErrExistsValueNotBool signals an "exists" condition whose Value is
// missing or not a bool. Without an explicit bool the previous behavior
// silently defaulted to false ("not exists"), turning a "drop if x
// exists" rule into "drop everything missing x". Validated in New so
// the misconfiguration fails the build.
var ErrExistsValueNotBool = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "filter: exists operator requires an explicit bool value",
}

// ErrInValueNotSlice signals an "in" condition whose Value is not a
// slice. A non-slice value can never match, silently disabling the
// condition. Validated in New so the misconfiguration fails the build.
var ErrInValueNotSlice = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "filter: in operator requires a slice value",
}

// ErrComparisonValueNotNumeric signals a gt/lt/gte/lte condition whose
// configured Value cannot be interpreted as a number. Validated in New
// so a deterministic misconfiguration fails the build instead of
// producing a per-message error the runtime would retry.
var ErrComparisonValueNotNumeric = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "filter: comparison operator requires a numeric value",
}

// DefaultMaxPayloadBytes bounds the JSON payload a filter will parse for
// a "$." path condition when Config.MaxPayloadBytes is left unset. It
// caps worst-case parse CPU for an oversized / hostile message while
// remaining generous for typical broker payloads.
const DefaultMaxPayloadBytes = 4 << 20 // 4 MiB

// Processor is a filter processor that evaluates conditions against an
// envelope and applies a configured action (pass, drop, or route).
type Processor struct {
	config     Config
	evaluators []*conditionEvaluator
}

var _ ports.Processor = (*Processor)(nil)

// New creates a filter processor from the given configuration.
// All regex patterns are pre-compiled during construction, and the
// action + every operator are validated so a misconfigured filter is
// rejected here rather than per message.
func New(cfg Config) (*Processor, error) {
	if err := validateAction(cfg.Action); err != nil {
		return nil, err
	}
	if cfg.MaxPayloadBytes <= 0 {
		cfg.MaxPayloadBytes = DefaultMaxPayloadBytes
	}

	p := &Processor{
		config:     cfg,
		evaluators: make([]*conditionEvaluator, 0, len(cfg.Conditions)),
	}

	for _, c := range cfg.Conditions {
		eval, err := newConditionEvaluator(c)
		if err != nil {
			return nil, err
		}
		eval.maxPayloadBytes = cfg.MaxPayloadBytes
		p.evaluators = append(p.evaluators, eval)
	}

	if cfg.Action == ActionRoute && cfg.RouteTo == "" {
		return nil, ErrRouteRequired
	}

	return p, nil
}

// validateAction reports whether a is a supported filter action.
func validateAction(a Action) error {
	switch a {
	case ActionPass, ActionDrop, ActionRoute:
		return nil
	default:
		return ErrUnknownAction.With("action", string(a))
	}
}

// contextError classifies an observed context cancellation / deadline as
// a transient processor timeout so the runtime may retry, while ensuring
// the processor stops promptly and does not mutate the envelope after
// the runtime has already moved on.
func contextError(err error) error {
	return shared.ErrProcessorTimeout.Wrap(err)
}

func (p *Processor) Name() string {
	if p.config.Name != "" {
		return p.config.Name
	}
	return "filter"
}

func (p *Processor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}

	matches, err := p.evaluate(ctx, env)
	if err != nil {
		return err
	}

	if p.config.Invert {
		matches = !matches
	}

	if !matches {
		return next(ctx, env)
	}

	switch p.config.Action {
	case ActionPass:
		return next(ctx, env)
	case ActionDrop:
		return shared.ErrMessageFiltered
	case ActionRoute:
		// Do not mutate the envelope if the runtime already timed out /
		// cancelled: it may have moved this envelope on already.
		if err := ctx.Err(); err != nil {
			return contextError(err)
		}
		if env.Headers() == nil {
			env.ReplaceHeaders(make(map[string]any, 1))
		}
		env.SetHeader(messaging.HeaderRouteOverride, p.config.RouteTo)
		return next(ctx, env)
	default:
		// Unreachable: validateAction rejects unknown actions in New.
		// Fail closed (never silently pass) as defence in depth for a
		// policy gate.
		return ErrUnknownAction.With("action", string(p.config.Action))
	}
}

func (p *Processor) evaluate(ctx context.Context, env *messaging.Envelope) (bool, error) {
	if len(p.evaluators) == 0 {
		return true, nil
	}

	// One payloadDoc per Process call: the JSON payload is copied and
	// parsed at most once and shared read-only across all "$." path
	// conditions.
	doc := &payloadDoc{}
	for _, eval := range p.evaluators {
		if err := ctx.Err(); err != nil {
			return false, contextError(err)
		}
		match, err := eval.evaluateDoc(env, doc)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}

	return true, nil
}

// NewDropFilter creates a filter that drops envelopes matching the conditions.
func NewDropFilter(name string, conditions ...Condition) (*Processor, error) {
	return New(Config{
		Name:       name,
		Conditions: conditions,
		Action:     ActionDrop,
	})
}

// NewPassFilter creates a filter that only passes envelopes matching the
// conditions; non-matching envelopes are dropped.
func NewPassFilter(name string, conditions ...Condition) (*Processor, error) {
	return New(Config{
		Name:       name,
		Conditions: conditions,
		Action:     ActionDrop,
		Invert:     true,
	})
}

// NewRouteFilter creates a filter that reroutes envelopes matching the conditions.
func NewRouteFilter(name string, routeTo string, conditions ...Condition) (*Processor, error) {
	return New(Config{
		Name:       name,
		Conditions: conditions,
		Action:     ActionRoute,
		RouteTo:    routeTo,
	})
}

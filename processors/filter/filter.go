package filter

import (
	"context"
	"errors"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var ErrRouteRequired = errors.New("route action requires routeTo configuration")

// Processor is a filter processor that evaluates conditions against an
// envelope and applies a configured action (pass, drop, or route).
type Processor struct {
	config     Config
	evaluators []*conditionEvaluator
}

var _ ports.Processor = (*Processor)(nil)

// New creates a filter processor from the given configuration.
// All regex patterns are pre-compiled during construction.
func New(cfg Config) (*Processor, error) {
	p := &Processor{
		config:     cfg,
		evaluators: make([]*conditionEvaluator, 0, len(cfg.Conditions)),
	}

	for _, c := range cfg.Conditions {
		eval, err := newConditionEvaluator(c)
		if err != nil {
			return nil, err
		}
		p.evaluators = append(p.evaluators, eval)
	}

	if cfg.Action == ActionRoute && cfg.RouteTo == "" {
		return nil, ErrRouteRequired
	}

	return p, nil
}

func (p *Processor) Name() string {
	if p.config.Name != "" {
		return p.config.Name
	}
	return "filter"
}

func (p *Processor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	matches, err := p.evaluate(env)
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
		if env.Headers() == nil {
			env.ReplaceHeaders(make(map[string]any, 1))
		}
		env.SetHeader(messaging.HeaderRouteOverride, p.config.RouteTo)
		return next(ctx, env)
	default:
		return next(ctx, env)
	}
}

func (p *Processor) evaluate(env *messaging.Envelope) (bool, error) {
	if len(p.evaluators) == 0 {
		return true, nil
	}

	for _, eval := range p.evaluators {
		match, err := eval.evaluate(env)
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

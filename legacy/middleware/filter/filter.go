package filter

import (
	"context"
	"errors"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ErrMessageDropped is returned when a message is filtered out.
var ErrMessageDropped = errors.New("message dropped by filter")

// ErrRouteRequired is returned when a route action is configured but no route is available.
var ErrRouteRequired = errors.New("route action requires routeTo configuration")

// Filter is a middleware that filters messages based on configurable conditions.
type Filter struct {
	config     Config
	evaluators []*conditionEvaluator
}

// New creates a new filter middleware with the given configuration.
func New(cfg Config) (*Filter, error) {
	f := &Filter{
		config:     cfg,
		evaluators: make([]*conditionEvaluator, 0, len(cfg.Conditions)),
	}

	// Pre-compile all condition evaluators
	for _, c := range cfg.Conditions {
		eval, err := newConditionEvaluator(c)
		if err != nil {
			return nil, err
		}
		f.evaluators = append(f.evaluators, eval)
	}

	// Validate config
	if cfg.Action == FilterActionRoute && cfg.RouteTo == "" {
		return nil, ErrRouteRequired
	}

	return f, nil
}

// Name returns the name of this middleware.
func (f *Filter) Name() string {
	if f.config.Name != "" {
		return f.config.Name
	}
	return "filter"
}

// Process evaluates the filter conditions and takes the configured action.
func (f *Filter) Process(ctx context.Context, msg *types.Message, next types.MiddlewareFunc) error {
	// Evaluate all conditions (AND logic)
	matches, err := f.evaluate(msg)
	if err != nil {
		return err
	}

	// Apply inversion if configured
	if f.config.Invert {
		matches = !matches
	}

	// If no match, continue to next middleware
	if !matches {
		return next(ctx, msg)
	}

	// Apply action based on configuration
	switch f.config.Action {
	case FilterActionPass:
		// Pass through to next middleware
		return next(ctx, msg)

	case FilterActionDrop:
		// Drop the message silently
		return nil

	case FilterActionRoute:
		// Store route target in metadata for router to handle
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata["_routeTo"] = f.config.RouteTo
		return next(ctx, msg)

	default:
		// Unknown action - pass through
		return next(ctx, msg)
	}
}

// evaluate checks if all conditions match the message.
func (f *Filter) evaluate(msg *types.Message) (bool, error) {
	// No conditions means always match
	if len(f.evaluators) == 0 {
		return true, nil
	}

	// All conditions must match (AND logic)
	for _, eval := range f.evaluators {
		match, err := eval.evaluate(msg)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}

	return true, nil
}

// Ensure Filter implements types.Middleware
var _ types.Middleware = (*Filter)(nil)

// NewDropFilter creates a filter that drops messages matching the conditions.
func NewDropFilter(name string, conditions ...Condition) (*Filter, error) {
	return New(Config{
		Name:       name,
		Conditions: conditions,
		Action:     FilterActionDrop,
	})
}

// NewPassFilter creates a filter that only passes messages matching the conditions.
// Messages not matching are dropped.
func NewPassFilter(name string, conditions ...Condition) (*Filter, error) {
	return New(Config{
		Name:       name,
		Conditions: conditions,
		Action:     FilterActionDrop,
		Invert:     true,
	})
}

// NewRouteFilter creates a filter that routes matching messages to a different target.
func NewRouteFilter(name string, routeTo string, conditions ...Condition) (*Filter, error) {
	return New(Config{
		Name:       name,
		Conditions: conditions,
		Action:     FilterActionRoute,
		RouteTo:    routeTo,
	})
}

package transform

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/ohler55/ojg/jp"
	"github.com/ohler55/ojg/oj"
)

// JSONTransform is a middleware that transforms message payloads using JSONPath.
type JSONTransform struct {
	config  Config
	parsers []parsedMapping
}

// parsedMapping holds a pre-parsed JSONPath expression and its mapping config.
type parsedMapping struct {
	mapping FieldMapping
	expr    jp.Expr
}

// New creates a new JSON transform middleware with the given configuration.
func New(cfg Config) (*JSONTransform, error) {
	t := &JSONTransform{
		config:  cfg,
		parsers: make([]parsedMapping, 0, len(cfg.Mappings)),
	}

	// Pre-parse all JSONPath expressions
	for _, m := range cfg.Mappings {
		expr, err := jp.ParseString(m.Source)
		if err != nil {
			return nil, fmt.Errorf("invalid JSONPath %q: %w", m.Source, err)
		}
		t.parsers = append(t.parsers, parsedMapping{
			mapping: m,
			expr:    expr,
		})
	}

	return t, nil
}

// Name returns the name of this middleware.
func (t *JSONTransform) Name() string {
	if t.config.Name != "" {
		return t.config.Name
	}
	return "json-transform"
}

// Process transforms the message payload according to the configured mappings.
func (t *JSONTransform) Process(ctx context.Context, msg *types.Message, next types.MiddlewareFunc) error {
	if len(msg.Payload) == 0 {
		return next(ctx, msg)
	}

	// Parse the payload
	data, err := oj.Parse(msg.Payload)
	if err != nil {
		// Not valid JSON - pass through
		return next(ctx, msg)
	}

	// Build output
	var output map[string]any
	if t.config.DropUnmapped {
		output = make(map[string]any)
	} else {
		// Start with original data
		if m, ok := data.(map[string]any); ok {
			output = m
		} else {
			// Wrap in object if not already
			output = map[string]any{"_value": data}
		}
	}

	// Apply mappings
	for _, pm := range t.parsers {
		if err := t.applyMapping(data, output, pm); err != nil {
			if t.config.FailOnError || pm.mapping.Required {
				return err
			}
			// Skip failed mapping
			continue
		}
	}

	// Serialize output
	newPayload, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("failed to serialize transformed payload: %w", err)
	}

	msg.Payload = newPayload
	return next(ctx, msg)
}

// applyMapping applies a single field mapping.
func (t *JSONTransform) applyMapping(data any, output map[string]any, pm parsedMapping) error {
	// Extract value using JSONPath
	results := pm.expr.Get(data)

	var value any
	if len(results) == 0 {
		// No match - use default or fail
		if pm.mapping.DefaultValue != nil {
			value = pm.mapping.DefaultValue
		} else if pm.mapping.Required {
			return fmt.Errorf("required field %q not found", pm.mapping.Source)
		} else {
			return nil // Skip this mapping
		}
	} else if len(results) == 1 {
		value = results[0]
	} else {
		// Multiple results - return as array
		value = results
	}

	// Apply transformation
	if pm.mapping.Transform != "" {
		var err error
		value, err = applyTransform(value, pm.mapping.Transform)
		if err != nil {
			return fmt.Errorf("transform failed for %q: %w", pm.mapping.Source, err)
		}
	}

	// Set in output
	setNestedValue(output, pm.mapping.Target, value)
	return nil
}

// Ensure JSONTransform implements types.Middleware
var _ types.Middleware = (*JSONTransform)(nil)

// SimpleMapping creates a simple source-to-target mapping.
func SimpleMapping(source, target string) FieldMapping {
	return FieldMapping{
		Source: source,
		Target: target,
	}
}

// RequiredMapping creates a required source-to-target mapping.
func RequiredMapping(source, target string) FieldMapping {
	return FieldMapping{
		Source:   source,
		Target:   target,
		Required: true,
	}
}

// TransformedMapping creates a mapping with a type transformation.
func TransformedMapping(source, target string, transform TransformType) FieldMapping {
	return FieldMapping{
		Source:    source,
		Target:    target,
		Transform: transform,
	}
}

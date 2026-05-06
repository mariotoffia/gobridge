package transform

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/ohler55/ojg/jp"
	"github.com/ohler55/ojg/oj"
)

// Processor is a processor that transforms message payloads using JSONPath.
type Processor struct {
	config  Config
	parsers []parsedMapping
}

// parsedMapping holds a pre-parsed JSONPath expression and its mapping config.
type parsedMapping struct {
	mapping FieldMapping
	expr    jp.Expr
}

// New creates a new JSON transform processor with the given configuration.
func New(cfg Config) (*Processor, error) {
	p := &Processor{
		config:  cfg,
		parsers: make([]parsedMapping, 0, len(cfg.Mappings)),
	}

	// Pre-parse all JSONPath expressions
	for _, m := range cfg.Mappings {
		expr, err := jp.ParseString(m.Source)
		if err != nil {
			return nil, fmt.Errorf("invalid JSONPath %q: %w", m.Source, err)
		}
		p.parsers = append(p.parsers, parsedMapping{
			mapping: m,
			expr:    expr,
		})
	}

	return p, nil
}

// Name returns the name of this processor.
func (p *Processor) Name() string {
	if p.config.Name != "" {
		return p.config.Name
	}
	return "transform"
}

// Process transforms the message payload according to the configured mappings.
func (p *Processor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	if len(env.Payload) == 0 {
		return next(ctx, env)
	}

	// Parse the payload
	data, err := oj.Parse(env.Payload)
	if err != nil {
		// Not valid JSON - pass through
		return next(ctx, env)
	}

	// Build output
	var output map[string]any
	if p.config.DropUnmapped {
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
	for _, pm := range p.parsers {
		if err := p.applyMapping(data, output, pm); err != nil {
			if p.config.FailOnError || pm.mapping.Required {
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

	env.Payload = newPayload
	return next(ctx, env)
}

// applyMapping applies a single field mapping.
func (p *Processor) applyMapping(data any, output map[string]any, pm parsedMapping) error {
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

// Ensure Processor implements ports.Processor
var _ ports.Processor = (*Processor)(nil)

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

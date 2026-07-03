package transform

import (
	"context"
	"fmt"

	"github.com/ohler55/ojg/jp"
	"github.com/ohler55/ojg/oj"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// DefaultMaxPayloadBytes bounds the JSON payload the transform will
// parse when Config.MaxPayloadBytes is unset. It caps worst-case parse
// CPU for an oversized / hostile message.
const DefaultMaxPayloadBytes = 4 << 20 // 4 MiB

// ErrHeaderTargetReserved signals that a mapping tried to write to a
// reserved x-bridge.* envelope header. Those headers are stripped at
// ingress as an anti-spoof measure, so writing them from a payload is
// rejected at construction. Setup error: permanent invalid-payload.
var ErrHeaderTargetReserved = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "transform: header target must not be a reserved x-bridge.* header",
}

// ErrHeaderTargetEmpty signals a "header." target with no header key.
// Setup error: permanent invalid-payload.
var ErrHeaderTargetEmpty = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "transform: header target key must not be empty",
}

// ErrUnknownTransformType signals a FieldMapping.Transform that is not
// one of the supported TransformType values. Validated at construction:
// with the default FailOnError=false a typo'd transform would otherwise
// silently skip its mapping on every message forever. Setup error:
// permanent invalid-payload.
var ErrUnknownTransformType = &shared.BridgeError{
	Code:    shared.ErrCodeInvalidPayload,
	Class:   shared.ErrorPermanent,
	Message: "transform: unknown transform type",
}

// Processor is a processor that transforms message payloads using JSONPath.
type Processor struct {
	config          Config
	parsers         []parsedMapping
	hasRequired     bool
	maxPayloadBytes int
}

// parsedMapping holds a pre-parsed JSONPath expression and its mapping config.
type parsedMapping struct {
	mapping FieldMapping
	expr    jp.Expr
	// headerKey is non-empty when the mapping Target addresses an
	// envelope header ("header.<headerKey>") rather than the payload.
	headerKey string
	toHeader  bool
}

// New creates a new JSON transform processor with the given configuration.
// JSONPath sources are pre-parsed and header targets are validated (a
// reserved x-bridge.* header target is rejected) so a misconfiguration
// fails fast at construction rather than per message.
func New(cfg Config) (*Processor, error) {
	if cfg.MaxPayloadBytes <= 0 {
		cfg.MaxPayloadBytes = DefaultMaxPayloadBytes
	}

	p := &Processor{
		config:          cfg,
		parsers:         make([]parsedMapping, 0, len(cfg.Mappings)),
		maxPayloadBytes: cfg.MaxPayloadBytes,
	}

	// Pre-parse all JSONPath expressions
	for _, m := range cfg.Mappings {
		expr, err := jp.ParseString(m.Source)
		if err != nil {
			return nil, shared.ErrInvalidPayload.Wrap(fmt.Errorf("invalid JSONPath %q: %w", m.Source, err))
		}
		if !validTransformType(m.Transform) {
			return nil, ErrUnknownTransformType.
				With("transform", string(m.Transform)).
				With("source", m.Source)
		}

		pm := parsedMapping{mapping: m, expr: expr}
		if key, ok := headerTarget(m.Target); ok {
			if key == "" {
				return nil, ErrHeaderTargetEmpty.With("target", m.Target)
			}
			if messaging.IsReservedHeader(key) {
				return nil, ErrHeaderTargetReserved.With("header", key)
			}
			pm.headerKey = key
			pm.toHeader = true
		}
		p.parsers = append(p.parsers, pm)

		if m.Required {
			p.hasRequired = true
		}
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
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}

	payload := env.Payload()
	if len(payload) == 0 {
		return next(ctx, env)
	}

	if len(payload) > p.maxPayloadBytes {
		return shared.ErrPayloadTooLarge.Wrap(
			fmt.Errorf("transform: payload %d bytes exceeds limit %d", len(payload), p.maxPayloadBytes))
	}

	// Parse the payload
	data, err := oj.Parse(payload)
	if err != nil {
		// A parse failure is only a legitimate pass-through when the
		// caller opted into best-effort (FailOnError=false) AND no
		// mapping is Required. Otherwise the payload cannot satisfy the
		// configured contract, so reject it (DLQ) instead of silently
		// forwarding it unchanged.
		if p.config.FailOnError || p.hasRequired {
			return shared.ErrInvalidPayload.
				WithMessage("transform: payload is not valid JSON").
				Wrap(err)
		}
		return next(ctx, env)
	}

	// Resolve header-target values over the pristine parsed data. Payload
	// mappings below also read only from the pristine data (they write to a
	// separate output object), so a header target always reads the ORIGINAL
	// payload value regardless of mapping order. Header writes are applied to
	// the envelope only after the final cancellation check, so a timed-out
	// message never mutates the envelope headers.
	var headerWrites map[string]any
	for _, pm := range p.parsers {
		if !pm.toHeader {
			continue
		}
		if err := ctx.Err(); err != nil {
			return contextError(err)
		}
		value, ok, err := p.resolveMapping(data, pm)
		if err != nil {
			if p.config.FailOnError || pm.mapping.Required {
				return shared.ErrInvalidPayload.Wrap(err)
			}
			continue
		}
		if !ok {
			continue
		}
		if headerWrites == nil {
			headerWrites = make(map[string]any, 1)
		}
		headerWrites[pm.headerKey] = value
	}

	// Apply payload mappings against an IMMUTABLE source: every mapping
	// reads from the pristine parsed data and writes into a separate
	// output object (a deep copy when DropUnmapped=false), so a mapping
	// chain never observes an earlier mapping's write — identical read
	// semantics in both DropUnmapped modes, matching how header targets
	// already resolve.
	var output map[string]any
	if p.config.DropUnmapped {
		output = make(map[string]any)
	}
	ensureOutput := func() map[string]any {
		if output == nil {
			if m, ok := data.(map[string]any); ok {
				output = deepCopyMap(m)
			} else {
				// A keyed mapping target needs an object root. A
				// non-object payload (array / scalar) is wrapped only
				// here — when a mapping genuinely writes — never for
				// header-only or non-matching mappings.
				output = map[string]any{"_value": data}
			}
		}
		return output
	}

	payloadApplied := false
	for _, pm := range p.parsers {
		if pm.toHeader {
			continue
		}
		if err := ctx.Err(); err != nil {
			return contextError(err)
		}
		value, ok, err := p.resolveMapping(data, pm)
		if err != nil {
			if p.config.FailOnError || pm.mapping.Required {
				// Transform failures are deterministic w.r.t. the
				// payload; a retry cannot succeed. Classify as rejected
				// so the runtime DLQs rather than retries.
				return shared.ErrInvalidPayload.Wrap(err)
			}
			// Skip failed mapping
			continue
		}
		if !ok {
			continue
		}
		setNestedValue(ensureOutput(), pm.mapping.Target, value)
		payloadApplied = true
	}

	// Serialize the output only when the configuration actually rewrote
	// the payload: DropUnmapped is an explicit whole-payload rewrite,
	// otherwise at least one mapping must have applied. Header-only and
	// no-op transforms leave the payload bytes byte-identical (no key
	// reordering, no HTML escaping, no {"_value": ...} wrapping of
	// non-object payloads).
	rewritePayload := p.config.DropUnmapped || payloadApplied
	var newPayload []byte
	if rewritePayload {
		newPayload, err = marshalPayload(output)
		if err != nil {
			return shared.ErrInvalidPayload.Wrap(fmt.Errorf("failed to serialize transformed payload: %w", err))
		}
	}

	// Final guard: do not mutate the envelope if the runtime already
	// cancelled/timed out — it may have moved this envelope on already.
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}

	if rewritePayload {
		env.SetPayload(newPayload)
	}
	for k, v := range headerWrites {
		env.SetHeader(k, v)
	}
	return next(ctx, env)
}

// contextError classifies an observed context cancellation / deadline as
// a transient processor timeout (runtime-retryable) while ensuring the
// processor stops promptly without further envelope mutation.
func contextError(err error) error {
	return shared.ErrProcessorTimeout.Wrap(err)
}

// resolveMapping computes the value a mapping would write (applying
// default / required / transform semantics) without touching any
// destination. ok is false when the mapping should be skipped (no match,
// not required, no default). It backs header-target mappings, which must
// stage their value rather than write into the payload object.
func (p *Processor) resolveMapping(data any, pm parsedMapping) (value any, ok bool, err error) {
	results := pm.expr.Get(data)

	switch {
	case len(results) == 0:
		switch {
		case pm.mapping.DefaultValue != nil:
			value = pm.mapping.DefaultValue
		case pm.mapping.Required:
			return nil, false, fmt.Errorf("required field %q not found", pm.mapping.Source)
		default:
			return nil, false, nil
		}
	case len(results) == 1:
		value = results[0]
	default:
		value = results
	}

	if pm.mapping.Transform != "" {
		value, err = applyTransform(value, pm.mapping.Transform)
		if err != nil {
			return nil, false, fmt.Errorf("transform failed for %q: %w", pm.mapping.Source, err)
		}
	}

	return value, true, nil
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

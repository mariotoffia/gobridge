package filter

// Action determines what happens when filter conditions match.
type Action string

const (
	ActionPass  Action = "pass"
	ActionDrop  Action = "drop"
	ActionRoute Action = "route"
)

// Config holds the configuration for a filter processor instance.
//
// The yaml/json tags define the serialized key names so a future
// configuration surface (file, blueprint, HTTP API) can decode directly
// into this struct — same convention as circuitbreaker.Config. No YAML
// decoding pipeline exists today; processors are constructed in Go and
// referenced by name from route definitions.
type Config struct {
	Name       string      `json:"name" yaml:"name"`
	Conditions []Condition `json:"conditions" yaml:"conditions"`
	Action     Action      `json:"action" yaml:"action"`
	RouteTo    string      `json:"routeTo,omitempty" yaml:"routeTo,omitempty"`
	Invert     bool        `json:"invert,omitempty" yaml:"invert,omitempty"`
	// MaxPayloadBytes bounds the JSON payload the filter is willing to
	// parse for a "$." path condition. Payloads larger than this are
	// rejected with shared.ErrPayloadTooLarge instead of being parsed,
	// bounding worst-case CPU for a hostile / oversized message.
	// Non-positive selects DefaultMaxPayloadBytes.
	MaxPayloadBytes int `json:"maxPayloadBytes,omitempty" yaml:"maxPayloadBytes,omitempty"`
}

// Condition defines a single field-level predicate.
//
// Supported Field patterns:
//   - "subject"       — matches Envelope.Subject
//   - "header.<key>"  — matches Envelope.Headers["<key>"]
//   - "$.<path>"      — dot-path traversal into JSON Envelope.Payload
//   - bare name       — falls back to Envelope.Headers lookup
type Condition struct {
	Field    string   `json:"field" yaml:"field"`
	Operator Operator `json:"operator" yaml:"operator"`
	Value    any      `json:"value" yaml:"value"`
}

// Operator defines the comparison operation for a Condition.
type Operator string

const (
	OperatorEquals         Operator = "eq"
	OperatorNotEquals      Operator = "ne"
	OperatorContains       Operator = "contains"
	OperatorRegex          Operator = "regex"
	OperatorGreaterThan    Operator = "gt"
	OperatorLessThan       Operator = "lt"
	OperatorGreaterOrEqual Operator = "gte"
	OperatorLessOrEqual    Operator = "lte"
	OperatorExists         Operator = "exists"
	OperatorIn             Operator = "in"
)

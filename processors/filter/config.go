package filter

// Action determines what happens when filter conditions match.
type Action string

const (
	ActionPass  Action = "pass"
	ActionDrop  Action = "drop"
	ActionRoute Action = "route"
)

// Config holds the configuration for a filter processor instance.
type Config struct {
	Name       string      `json:"name"`
	Conditions []Condition `json:"conditions"`
	Action     Action      `json:"action"`
	RouteTo    string      `json:"routeTo,omitempty"`
	Invert     bool        `json:"invert,omitempty"`
}

// Condition defines a single field-level predicate.
//
// Supported Field patterns:
//   - "subject"       — matches Envelope.Subject
//   - "header.<key>"  — matches Envelope.Headers["<key>"]
//   - "$.<path>"      — dot-path traversal into JSON Envelope.Payload
//   - bare name       — falls back to Envelope.Headers lookup
type Condition struct {
	Field    string   `json:"field"`
	Operator Operator `json:"operator"`
	Value    any      `json:"value"`
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

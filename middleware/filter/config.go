package filter

// FilterAction defines what action to take when a filter matches.
type FilterAction string

const (
	// FilterActionPass allows the message to continue through the pipeline.
	FilterActionPass FilterAction = "pass"
	// FilterActionDrop silently drops the message.
	FilterActionDrop FilterAction = "drop"
	// FilterActionRoute routes the message to a different target.
	FilterActionRoute FilterAction = "route"
)

// Config holds the configuration for the filter middleware.
type Config struct {
	// Name is the unique name for this middleware instance.
	Name string `json:"name"`
	// Conditions to match. All conditions must match (AND logic).
	Conditions []Condition `json:"conditions"`
	// Action to take when filter matches.
	Action FilterAction `json:"action"`
	// RouteTo specifies the target for "route" action.
	RouteTo string `json:"routeTo,omitempty"`
	// Invert negates the filter result. If true, the action is taken when
	// the conditions do NOT match.
	Invert bool `json:"invert,omitempty"`
}

// Condition defines a single filter condition.
type Condition struct {
	// Field is the field to check. Supported values:
	// - "topic" - the message topic
	// - "metadata.<key>" - a metadata field
	// - "$.<path>" - JSONPath expression for payload fields
	Field string `json:"field"`
	// Operator is the comparison operator.
	Operator Operator `json:"operator"`
	// Value is the value to compare against.
	Value any `json:"value"`
}

// Operator defines the comparison operator for conditions.
type Operator string

const (
	// OperatorEquals checks for equality.
	OperatorEquals Operator = "eq"
	// OperatorNotEquals checks for inequality.
	OperatorNotEquals Operator = "ne"
	// OperatorContains checks if the field contains the value (string).
	OperatorContains Operator = "contains"
	// OperatorRegex checks if the field matches the regex pattern.
	OperatorRegex Operator = "regex"
	// OperatorGreaterThan checks if the field is greater than the value.
	OperatorGreaterThan Operator = "gt"
	// OperatorLessThan checks if the field is less than the value.
	OperatorLessThan Operator = "lt"
	// OperatorGreaterOrEqual checks if the field is greater than or equal to the value.
	OperatorGreaterOrEqual Operator = "gte"
	// OperatorLessOrEqual checks if the field is less than or equal to the value.
	OperatorLessOrEqual Operator = "lte"
	// OperatorExists checks if the field exists.
	OperatorExists Operator = "exists"
	// OperatorIn checks if the field value is in a list.
	OperatorIn Operator = "in"
)

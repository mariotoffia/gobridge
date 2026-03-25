package transform

// Config holds the configuration for the JSON transform middleware.
type Config struct {
	// Name is the unique name for this middleware instance.
	Name string `json:"name"`
	// Mappings define field transformations.
	Mappings []FieldMapping `json:"mappings"`
	// DropUnmapped removes fields not in mappings (creates new object).
	// If false (default), original fields are preserved and mappings are applied on top.
	DropUnmapped bool `json:"dropUnmapped,omitempty"`
	// FailOnError stops processing if a mapping fails. Default is false (skip failed mappings).
	FailOnError bool `json:"failOnError,omitempty"`
}

// FieldMapping defines a single field transformation.
type FieldMapping struct {
	// Source is the JSONPath expression to extract the source value.
	// Example: "$.user.name", "$.items[0].id"
	Source string `json:"source"`
	// Target is the target field path (dot notation).
	// Example: "userName", "firstItemId", "nested.field"
	Target string `json:"target"`
	// Transform is an optional transformation to apply.
	// Supported: "string", "int", "float", "bool", "base64encode", "base64decode"
	Transform TransformType `json:"transform,omitempty"`
	// DefaultValue is the value to use if source is not found.
	DefaultValue any `json:"defaultValue,omitempty"`
	// Required indicates the mapping must succeed or the message processing fails.
	Required bool `json:"required,omitempty"`
}

// TransformType defines the type of value transformation.
type TransformType string

const (
	// TransformNone applies no transformation.
	TransformNone TransformType = ""
	// TransformString converts the value to a string.
	TransformString TransformType = "string"
	// TransformInt converts the value to an integer.
	TransformInt TransformType = "int"
	// TransformFloat converts the value to a float.
	TransformFloat TransformType = "float"
	// TransformBool converts the value to a boolean.
	TransformBool TransformType = "bool"
	// TransformBase64Encode encodes the value as base64.
	TransformBase64Encode TransformType = "base64encode"
	// TransformBase64Decode decodes the value from base64.
	TransformBase64Decode TransformType = "base64decode"
)

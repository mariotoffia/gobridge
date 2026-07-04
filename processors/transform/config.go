package transform

// Config holds the configuration for the JSON transform middleware.
//
// The yaml/json tags define the serialized key names so a future
// configuration surface (file, blueprint, HTTP API) can decode directly
// into this struct — same convention as circuitbreaker.Config. No YAML
// decoding pipeline exists today; processors are constructed in Go and
// referenced by name from route definitions.
type Config struct {
	// Name is the unique name for this middleware instance.
	Name string `json:"name" yaml:"name"`
	// Mappings define field transformations.
	Mappings []FieldMapping `json:"mappings" yaml:"mappings"`
	// DropUnmapped removes fields not in mappings (creates new object).
	// If false (default), original fields are preserved and mappings are applied on top.
	DropUnmapped bool `json:"dropUnmapped,omitempty" yaml:"dropUnmapped,omitempty"`
	// FailOnError stops processing if a mapping fails. Default is false (skip failed mappings).
	FailOnError bool `json:"failOnError,omitempty" yaml:"failOnError,omitempty"`
	// MaxPayloadBytes bounds the JSON payload this processor will parse.
	// A payload larger than this is rejected before parsing to cap
	// worst-case CPU. Non-positive selects DefaultMaxPayloadBytes.
	MaxPayloadBytes int `json:"maxPayloadBytes,omitempty" yaml:"maxPayloadBytes,omitempty"`
}

// FieldMapping defines a single field transformation.
type FieldMapping struct {
	// Source is the JSONPath expression to extract the source value.
	// Example: "$.user.name", "$.items[0].id"
	Source string `json:"source" yaml:"source"`
	// Target is the target field path (dot notation).
	// Example: "userName", "firstItemId", "nested.field"
	//
	// A Target prefixed with "header." writes the mapped value to an
	// envelope header instead of the payload. Example: "header.x-tenant"
	// copies the parsed value into the "x-tenant" header. Reserved
	// x-bridge.* header targets are rejected at construction.
	Target string `json:"target" yaml:"target"`
	// Transform is an optional transformation to apply.
	// Supported: "string", "int", "float", "bool", "base64encode", "base64decode"
	Transform TransformType `json:"transform,omitempty" yaml:"transform,omitempty"`
	// DefaultValue is the value to use if source is not found.
	DefaultValue any `json:"defaultValue,omitempty" yaml:"defaultValue,omitempty"`
	// Required indicates the mapping must succeed or the message processing fails.
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`
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

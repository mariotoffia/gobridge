package ports

import (
	"fmt"
	"strings"
)

// BlueprintValidationError collects multiple validation problems from
// a blueprint validator. It carries hard errors and non-fatal
// warnings; the caller treats Errors as failure conditions and
// Warnings as advisory.
//
// The validator implementation in the config parser package
// constructs and returns a *BlueprintValidationError; admin layers
// (httpapi) inspect Warnings/Errors directly without depending on
// the parser package.
type BlueprintValidationError struct {
	Errors   []string
	Warnings []string
}

// Error implements the error interface. The format mirrors what the
// in-process config parser used to produce so existing log/test
// matchers continue to fire.
func (e *BlueprintValidationError) Error() string {
	return "config validation failed:\n  " + strings.Join(e.Errors, "\n  ")
}

// HasErrors reports whether the validator captured any hard errors.
func (e *BlueprintValidationError) HasErrors() bool {
	return len(e.Errors) > 0
}

// Add appends a hard error message.
func (e *BlueprintValidationError) Add(msg string) {
	e.Errors = append(e.Errors, msg)
}

// Addf appends a formatted hard error message.
func (e *BlueprintValidationError) Addf(format string, args ...any) {
	e.Errors = append(e.Errors, fmt.Sprintf(format, args...))
}

// Warnf appends a formatted advisory warning.
func (e *BlueprintValidationError) Warnf(format string, args ...any) {
	e.Warnings = append(e.Warnings, fmt.Sprintf(format, args...))
}

// ConfigStore is the boundary the admin HTTP layer consumes for the
// load/save/validate/merge operations on a *BridgeConfig. It lets
// httpapi avoid a direct dependency on the config parser package; a
// composition root supplies an implementation backed by config or by
// any other source (DynamoDB, Vault, etc.).
type ConfigStore interface {
	// Load returns the current parsed blueprint from the underlying
	// store (e.g. on-disk YAML).
	Load() (*BridgeConfig, error)

	// Save persists the blueprint to the underlying store.
	Save(cfg *BridgeConfig) error

	// Validate performs structural validation. Returns the warnings
	// the validator captured even when err is nil so the admin layer
	// can surface advisory issues to the operator.
	Validate(cfg *BridgeConfig) (warnings []string, err error)

	// Merge combines an overlay blueprint on top of a base, returning
	// a new value (the inputs are not mutated).
	Merge(base, overlay *BridgeConfig) (*BridgeConfig, error)
}

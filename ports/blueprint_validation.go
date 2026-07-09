package ports

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrApplyInFlight is the single "committed, NOT confirmed applied, DO NOT roll
// back" terminal signal of an in-band config apply. It lives in ports (not the
// composition root) so the admin config-transaction layer in the httpapi module
// can errors.Is against it — the whole rollback decision hinges on recognising
// it across the module boundary.
//
// The in-band applier (cmd/gobridge) feeds a committed config to the runtime
// Supervisor and reports one of exactly three terminal outcomes:
//
//   - err == nil                       → swap SUCCEEDED; the runtime runs cfg.
//     Do NOT roll back.
//   - errors.Is(err, ErrApplyInFlight) → cfg was accepted by the runtime but its
//     running state is NOT confirmed: the swap is still in-flight past the apply
//     deadline, OR the bridge is paused/shutting down and recorded cfg for a
//     later resume. In every such case cfg is (or will become) the durable
//     desired state, so rolling the durable config back would fight the runtime
//     — Do NOT roll back. Surface committed_not_applied and let the operator
//     reconcile; the file watcher / next swap converges the observable state.
//   - any other err != nil             → DEFINITIVE FAILURE; cfg never reached a
//     running apply, or the swap was rejected and the Supervisor recovered the
//     OLD config. The runtime is NOT on cfg, so rolling the durable config back
//     is SAFE and consistent.
//
// There is no ambiguous "maybe-failed" outcome: every result is one of the
// three above.
var ErrApplyInFlight = errors.New(
	"config apply in-flight: committed but running state not confirmed; do not roll back")

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
	// store (e.g. on-disk YAML). ctx is honoured for cancellation.
	Load(ctx context.Context) (*BridgeConfig, error)

	// Save persists the blueprint to the underlying store. ctx is
	// honoured for cancellation.
	Save(ctx context.Context, cfg *BridgeConfig) error

	// Validate performs structural validation. Returns the warnings
	// the validator captured even when err is nil so the admin layer
	// can surface advisory issues to the operator.
	Validate(ctx context.Context, cfg *BridgeConfig) (warnings []string, err error)

	// Merge combines an overlay blueprint on top of a base, returning
	// a new value (the inputs are not mutated).
	Merge(ctx context.Context, base, overlay *BridgeConfig) (*BridgeConfig, error)
}

// ConditionalConfigStore is an OPTIONAL ConfigStore capability. A store that
// implements it supports compare-and-swap saves so concurrent admin instances
// committing against a shared backend cannot clobber each other's work with a
// last-writer-wins race (see BridgeConfig.Version).
//
// Callers detect support with a type assertion and fall back to the plain
// Save (documenting the last-writer-wins caveat) when the backing store does
// not implement it.
type ConditionalConfigStore interface {
	ConfigStore

	// SaveIfVersion persists cfg only when the store's currently-persisted
	// version equals expectedVersion, then advances the stored version.
	// On a mismatch it writes nothing and returns shared.ErrVersionMismatch,
	// so the caller can reload the current config and re-apply its edit.
	// expectedVersion is the BridgeConfig.Version the caller based its edit
	// on; a store with no prior config treats expectedVersion == 0 as
	// "create if absent". ctx is honoured for cancellation.
	SaveIfVersion(ctx context.Context, cfg *BridgeConfig, expectedVersion int) error
}

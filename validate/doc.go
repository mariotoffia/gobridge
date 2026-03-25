// Package validate provides startup configuration validation for the
// GoBridge runtime. It checks route, session, binding, and store wiring
// before any messages flow and produces clear, actionable error messages
// for every invalid combination.
//
// The validator is intentionally separated from the runtime package so
// that both the runtime and external tools can validate bridge configs
// without importing application-service code.
//
// Dependency direction: validate imports domain and ports only.
package validate

// Package logging is the slog-based logging facade used across the
// codebase. Centralising logger construction keeps level handling,
// trace-level extension, and attribute conventions consistent between
// the runtime, adapters, and processors.
//
// Responsibility:
//   - expose a thin wrapper over log/slog that adds a TRACE level and
//     a TraceEnabled helper for hot-path guard checks
//   - provide a single construction surface so consumers do not reach
//     directly for the slog package and accidentally diverge on
//     handler / level configuration
//
// Key types:
//   - Logger: the slog.Logger-compatible facade
//   - LevelTrace and TraceEnabled: extended verbosity below DEBUG
//
// Dependencies: stdlib only (log/slog). Imported by runtime, adapters,
// and processors; depends on none of them.
package logging

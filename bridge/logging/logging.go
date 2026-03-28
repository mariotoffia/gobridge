// Package logging provides trace and debug level support on top of Go's
// standard log/slog. It defines a Trace level below slog.LevelDebug and
// provides zero-cost level guards so callers can skip expensive argument
// evaluation when the level is disabled.
//
// Usage:
//
//	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
//	    Level: logging.LevelTrace,
//	}))
//
//	if logging.TraceEnabled(logger) {
//	    logger.Log(ctx, logging.LevelTrace, "msg received",
//	        "payload_len", len(payload))
//	}
//
// The guard pattern prevents allocation of log attributes when the level
// is disabled, which matters in hot paths like message delivery loops.
package logging

import (
	"context"
	"log/slog"
)

// Custom slog levels. slog.LevelDebug is -4; Trace sits below it.
const (
	// LevelTrace is the most verbose level, below slog.LevelDebug.
	// Use for per-message flow tracing, slot acquisition, header dumps.
	LevelTrace = slog.Level(-8)

	// LevelDebug is an alias for slog.LevelDebug (-4).
	// Use for component lifecycle events, batch operations, resolver decisions.
	LevelDebug = slog.LevelDebug
)

// TraceEnabled returns true if the logger accepts Trace-level records.
// Use as a guard before constructing expensive log arguments:
//
//	if logging.TraceEnabled(logger) {
//	    logger.Log(ctx, logging.LevelTrace, "detail", "key", expensiveValue())
//	}
func TraceEnabled(l *slog.Logger) bool {
	if l == nil {
		return false
	}
	return l.Enabled(context.Background(), LevelTrace)
}

// DebugEnabled returns true if the logger accepts Debug-level records.
func DebugEnabled(l *slog.Logger) bool {
	if l == nil {
		return false
	}
	return l.Enabled(context.Background(), LevelDebug)
}

// Trace logs at LevelTrace if the logger is non-nil and the level is enabled.
// This is a convenience wrapper that includes the nil check and level guard.
func Trace(l *slog.Logger, msg string, args ...any) {
	if l != nil && l.Enabled(context.Background(), LevelTrace) {
		l.Log(context.Background(), LevelTrace, msg, args...)
	}
}

// Debug logs at LevelDebug if the logger is non-nil and the level is enabled.
func Debug(l *slog.Logger, msg string, args ...any) {
	if l != nil && l.Enabled(context.Background(), LevelDebug) {
		l.Log(context.Background(), LevelDebug, msg, args...)
	}
}

// LevelNames maps custom level values to human-readable names.
// Use with slog.HandlerOptions.ReplaceAttr to display "TRACE" instead of
// "DEBUG-4":
//
//	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
//	    Level: logging.LevelTrace,
//	    ReplaceAttr: logging.ReplaceLevel,
//	})
func ReplaceLevel(_ []string, a slog.Attr) slog.Attr {
	if a.Key != slog.LevelKey {
		return a
	}
	level, ok := a.Value.Any().(slog.Level)
	if !ok {
		return a
	}
	if level <= LevelTrace {
		a.Value = slog.StringValue("TRACE")
	}
	return a
}

// NewLogger creates a logger with proper level name rendering.
// handler is the underlying slog.Handler (e.g., slog.NewTextHandler).
// This is a convenience for tests and main functions.
func NewLogger(level slog.Level, handler func(*slog.HandlerOptions) slog.Handler) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: ReplaceLevel,
	}
	return slog.New(handler(opts))
}

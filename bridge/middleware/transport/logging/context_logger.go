package logging

import (
	"context"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ContextLogger wraps a Logger and automatically injects correlation and trace IDs.
// It extracts IDs from the context and includes them in all log entries.
type ContextLogger struct {
	logger types.Logger
	ctx    context.Context
}

// NewContextLogger creates a new context-aware logger wrapper.
// It extracts correlation IDs, trace IDs, and span IDs from the context
// and automatically includes them in all log entries.
func NewContextLogger(ctx context.Context, logger types.Logger) *ContextLogger {
	return &ContextLogger{
		logger: logger,
		ctx:    ctx,
	}
}

// withIDs returns a logger with correlation and trace IDs injected.
func (l *ContextLogger) withIDs() types.Logger {
	logger := l.logger

	// Inject correlation ID
	if correlationID := GetCorrelationID(l.ctx); correlationID != "" {
		logger = logger.Str("correlationId", correlationID)
	}

	// Inject trace ID
	if traceID := GetTraceID(l.ctx); traceID != "" {
		logger = logger.Str("traceId", traceID)
	}

	// Inject span ID
	if spanID := GetSpanID(l.ctx); spanID != "" {
		logger = logger.Str("spanId", spanID)
	}

	return logger
}

// WhenLevel executes the provided function if the log level is sufficient.
func (l *ContextLogger) WhenLevel(level types.LogLevel, fn func(l types.Logger)) types.Logger {
	l.withIDs().WhenLevel(level, fn)
	return l
}

// WithMethod adds a method name to the logger.
func (l *ContextLogger) WithMethod(method string) types.Logger {
	return &ContextLogger{
		logger: l.logger.WithMethod(method),
		ctx:    l.ctx,
	}
}

// WithService adds a service name to the logger.
func (l *ContextLogger) WithService(service string) types.Logger {
	return &ContextLogger{
		logger: l.logger.WithService(service),
		ctx:    l.ctx,
	}
}

// AsJSON adds a JSON-encoded value to the logger.
func (l *ContextLogger) AsJSON(key string, value any) types.Logger {
	return &ContextLogger{
		logger: l.logger.AsJSON(key, value),
		ctx:    l.ctx,
	}
}

// Str adds a string field to the logger.
func (l *ContextLogger) Str(key, value string) types.Logger {
	return &ContextLogger{
		logger: l.logger.Str(key, value),
		ctx:    l.ctx,
	}
}

// Int adds an integer field to the logger.
func (l *ContextLogger) Int(key string, value int) types.Logger {
	return &ContextLogger{
		logger: l.logger.Int(key, value),
		ctx:    l.ctx,
	}
}

// Bool adds a boolean field to the logger.
func (l *ContextLogger) Bool(key string, value bool) types.Logger {
	return &ContextLogger{
		logger: l.logger.Bool(key, value),
		ctx:    l.ctx,
	}
}

// Error adds an error to the logger.
func (l *ContextLogger) Error(err error) types.Logger {
	return &ContextLogger{
		logger: l.logger.Error(err),
		ctx:    l.ctx,
	}
}

// Msg logs a message with all accumulated fields and context IDs.
func (l *ContextLogger) Msg(msg string) {
	l.withIDs().Msg(msg)
}

// Msgf logs a formatted message with all accumulated fields and context IDs.
func (l *ContextLogger) Msgf(format string, args ...any) {
	l.withIDs().Msgf(format, args...)
}

// SetGlobalLevel sets the global log level.
func (l *ContextLogger) SetGlobalLevel(level types.LogLevel) types.Logger {
	l.logger.SetGlobalLevel(level)
	return l
}

// Ensure ContextLogger implements types.Logger
var _ types.Logger = (*ContextLogger)(nil)

// ============================================================================
// Logger Factory Functions
// ============================================================================

// LoggerFromContext creates a context-aware logger from the given context.
// If the context contains correlation/trace IDs, they will be included in logs.
func LoggerFromContext(ctx context.Context, logger types.Logger) types.Logger {
	return NewContextLogger(ctx, logger)
}

// LoggerWithIDs creates a logger with the specified IDs pre-configured.
// This is useful when you need to create a logger without a context.
func LoggerWithIDs(logger types.Logger, lc LogContext) types.Logger {
	if lc.CorrelationID != "" {
		logger = logger.Str("correlationId", lc.CorrelationID)
	}
	if lc.TraceID != "" {
		logger = logger.Str("traceId", lc.TraceID)
	}
	if lc.SpanID != "" {
		logger = logger.Str("spanId", lc.SpanID)
	}
	return logger
}

// ============================================================================
// Convenience Functions
// ============================================================================

// Debug logs a debug message with context IDs.
func Debug(ctx context.Context, logger types.Logger, msg string) {
	NewContextLogger(ctx, logger).Msg(msg)
}

// Info logs an info message with context IDs.
func Info(ctx context.Context, logger types.Logger, msg string) {
	NewContextLogger(ctx, logger).Msg(msg)
}

// Warn logs a warning message with context IDs.
func Warn(ctx context.Context, logger types.Logger, msg string) {
	NewContextLogger(ctx, logger).Msg(msg)
}

// ErrorLog logs an error message with context IDs.
func ErrorLog(ctx context.Context, logger types.Logger, err error, msg string) {
	NewContextLogger(ctx, logger).Error(err).Msg(msg)
}

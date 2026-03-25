package types

import "context"

type LogLevel int

const (
	// LogLevelTrace represents trace-level log messages.
	LogLevelTrace LogLevel = 1
	// LogLevelDebug represents debug-level log messages.
	LogLevelDebug LogLevel = 2
	// LogLevelInfo represents informational log messages.
	LogLevelInfo LogLevel = 3
	// LogLevelWarn represents warning-level log messages.
	LogLevelWarn LogLevel = 4
	// LogLevelError represents error-level log messages.
	LogLevelError LogLevel = 5
)

// LoggerFactory creates component-bound LogCreators.
// The component name (e.g., "bridge", "pipeline:foo", "mqtt-source:bar") is baked
// into the returned LogCreator and cannot be changed.
//
// Usage:
//
//	factory := logging.NewSlogFactory(slog.Default())
//	bridgeLog := factory("bridge")
//	bridgeLog(ctx, types.LogLevelInfo).Str("id", id).Msg("starting")
type LoggerFactory func(component string) LogCreator

// LogCreator is a factory function type for creating Logger instances.
// Each call creates a fresh Logger for a single log message.
// The component name is already baked into the LogCreator.
type LogCreator func(ctx context.Context, level LogLevel) Logger

// Logger is an interface for logging messages at various severity levels.
// A Logger instance is created per log message using LogCreator.
// Attributes are chained, then Msg() or Msgf() is called to emit the log.
//
// Example:
//
//	log(ctx, types.LogLevelInfo).Str("key", "value").Msg("message")
type Logger interface {
	// WhenLevel executes the provided function if the current log level
	// is equal to or higher than the specified level.
	WhenLevel(level LogLevel, fn func(l Logger)) Logger
	// AsJSON adds a JSON-serialized value to the log entry.
	AsJSON(key string, value any) Logger
	// Str adds a string key-value pair to the log entry.
	Str(key, value string) Logger
	// Int adds an integer key-value pair to the log entry.
	Int(key string, value int) Logger
	// Bool adds a boolean key-value pair to the log entry.
	Bool(key string, value bool) Logger
	// Err adds an error to the log entry.
	Err(err error) Logger
	// Msg emits the log message with all accumulated attributes.
	Msg(msg string)
	// Msgf emits a formatted log message with all accumulated attributes.
	Msgf(format string, args ...any)
	// SetGlobalLevel sets the global log level for all loggers.
	//
	// CAUTION: Use this with caution as it affects all logging.
	SetGlobalLevel(level LogLevel) Logger
}

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

// LogCreator is a factory function type for creating Logger instances based on the specified LogLevel.
type LogCreator func(ctx context.Context, level LogLevel) Logger

// Logger is an interface for logging messages at various severity levels.
//
// It is used to allow different logging implementations to be plugged into the system.
type Logger interface {
	WithMethod(method string) Logger
	WithService(service string) Logger
	AsJSON(key string, value any) Logger
	Str(key, value string) Logger
	Int(key string, value int) Logger
	Bool(key string, value bool) Logger
	Error(err error) Logger
	Msg(msg string)
	Msgf(format string, args ...any)
}

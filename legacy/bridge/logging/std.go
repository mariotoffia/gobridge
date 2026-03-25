package logging

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/bridge/types"
)

var globalLevel int32 = int32(slog.LevelInfo)

// StandardLogger is a basic implementation of the Logger interface using golang standard log package.
// The component name is immutable and set at creation time via the LoggerFactory.
type StandardLogger struct {
	logger    *slog.Logger
	component string
	attrs     []slog.Attr
	level     types.LogLevel
	ctx       context.Context
}

// NewSlogFactory creates a LoggerFactory backed by slog.
// The returned factory creates component-bound LogCreators where the component
// name is automatically added to every log message.
//
// Example:
//
//	factory := logging.NewSlogFactory(slog.Default())
//	bridgeLog := factory("bridge")
//	bridgeLog(ctx, types.LogLevelInfo).Str("id", "123").Msg("starting")
//	// Output: level=INFO msg=starting component=bridge id=123
func NewSlogFactory(base *slog.Logger) types.LoggerFactory {
	return func(component string) types.LogCreator {
		return func(ctx context.Context, level types.LogLevel) types.Logger {
			return &StandardLogger{
				logger:    base,
				component: component,
				level:     level,
				ctx:       ctx,
			}
		}
	}
}

// NewSlogCreator creates a LogCreator backed by slog without a component name.
// Prefer NewSlogFactory for component-aware logging.
//
// Deprecated: Use NewSlogFactory instead for component-aware logging.
func NewSlogCreator(base *slog.Logger) types.LogCreator {
	return func(ctx context.Context, level types.LogLevel) types.Logger {
		return &StandardLogger{logger: base, level: level, ctx: ctx}
	}
}

func (l *StandardLogger) SetGlobalLevel(level types.LogLevel) types.Logger {
	slv := toSlogLevel(level)

	atomic.StoreInt32(&globalLevel, int32(slv))
	slog.SetLogLoggerLevel(slv)

	return l
}

func (l *StandardLogger) WhenLevel(level types.LogLevel, fn func(l types.Logger)) types.Logger {
	if l.level <= types.LogLevel(atomic.LoadInt32(&globalLevel)) {
		fn(l)
	}

	return l
}

func (l *StandardLogger) AsJSON(key string, value any) types.Logger {
	l.attrs = append(l.attrs, slog.Any(key, value))
	return l
}

func (l *StandardLogger) Str(key, value string) types.Logger {
	l.attrs = append(l.attrs, slog.String(key, value))
	return l
}

func (l *StandardLogger) Int(key string, value int) types.Logger {
	l.attrs = append(l.attrs, slog.Int(key, value))
	return l
}

func (l *StandardLogger) Bool(key string, value bool) types.Logger {
	l.attrs = append(l.attrs, slog.Bool(key, value))
	return l
}

func (l *StandardLogger) Err(err error) types.Logger {
	if err != nil {
		l.attrs = append(l.attrs, slog.Any("error", err))
	}
	return l
}

func (l *StandardLogger) Msg(msg string) {
	attrs := l.buildAttrs()
	l.logger.LogAttrs(l.ctx, toSlogLevel(l.level), msg, attrs...)
}

func (l *StandardLogger) Msgf(format string, args ...any) {
	attrs := l.buildAttrs()
	l.logger.LogAttrs(l.ctx, toSlogLevel(l.level), fmt.Sprintf(format, args...), attrs...)
}

// buildAttrs prepends the component attribute if set.
func (l *StandardLogger) buildAttrs() []slog.Attr {
	if l.component == "" {
		return l.attrs
	}
	// Prepend component so it appears first in log output
	attrs := make([]slog.Attr, 0, len(l.attrs)+1)
	attrs = append(attrs, slog.String("component", l.component))
	attrs = append(attrs, l.attrs...)
	return attrs
}

func toSlogLevel(level types.LogLevel) slog.Level {
	switch level {
	case types.LogLevelTrace:
		return slog.LevelDebug - 4 // slog doesn't have trace, use debug-4
	case types.LogLevelDebug:
		return slog.LevelDebug
	case types.LogLevelInfo:
		return slog.LevelInfo
	case types.LogLevelWarn:
		return slog.LevelWarn
	case types.LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

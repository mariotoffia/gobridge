package logging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// StandardLogger is a basic implementation of the Logger interface using golang standard log package.
type StandardLogger struct {
	logger *slog.Logger
	attrs  []slog.Attr
	level  types.LogLevel
	ctx    context.Context
}

func NewSlogCreator(base *slog.Logger) types.LogCreator {
	return func(ctx context.Context, level types.LogLevel) types.Logger {
		return &StandardLogger{logger: base, level: level, ctx: ctx}
	}
}

func (l *StandardLogger) WithMethod(method string) types.Logger {
	l.attrs = append(l.attrs, slog.String("method", method))
	return l
}
func (l *StandardLogger) WithService(service string) types.Logger {
	l.attrs = append(l.attrs, slog.String("service", service))
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
func (l *StandardLogger) Error(err error) types.Logger {
	l.attrs = append(l.attrs, slog.Any("error", err))
	return l
}
func (l *StandardLogger) Msg(msg string) {
	l.logger.LogAttrs(l.ctx, l.toSlogLevel(), msg, l.attrs...)
}
func (l *StandardLogger) Msgf(format string, args ...any) {
	l.logger.LogAttrs(l.ctx, l.toSlogLevel(), fmt.Sprintf(format, args...), l.attrs...)
}

func (l *StandardLogger) toSlogLevel() slog.Level {
	switch l.level {
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

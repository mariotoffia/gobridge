package ports

import (
	"log/slog"
	"slices"
	"strings"
)

// logLevelNames maps every accepted spelling of bridge.log_level to its
// slog.Level. It is the single definition of the enum: the blueprint validator
// rejects what is not in here, and every composition root resolves through
// ParseLogLevel, so a value that validates always applies and a value that
// applies always validates. "warning" is an accepted alias of "warn".
//
//nolint:gochecknoglobals // immutable enum table; Go has no const map
var logLevelNames = map[string]slog.Level{
	"debug":   slog.LevelDebug,
	"info":    slog.LevelInfo,
	"warn":    slog.LevelWarn,
	"warning": slog.LevelWarn,
	"error":   slog.LevelError,
}

// ParseLogLevel maps a bridge.log_level (or -log-level) string to its
// slog.Level. Matching is case-insensitive and ignores surrounding whitespace.
// The bool is false for an empty or unrecognised value so a caller can keep the
// level it already has rather than silently resetting verbosity to info.
func ParseLogLevel(s string) (slog.Level, bool) {
	lvl, ok := logLevelNames[strings.ToLower(strings.TrimSpace(s))]
	return lvl, ok
}

// LogLevelNames returns the accepted log-level spellings in sorted order, for
// validation messages and generated documentation.
func LogLevelNames() []string {
	out := make([]string, 0, len(logLevelNames))
	for name := range logLevelNames {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

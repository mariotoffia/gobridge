package ports_test

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// TestParseLogLevel pins the accepted spellings of bridge.log_level (and the
// -log-level flag): case-insensitive, whitespace-tolerant, with "warning" as an
// alias of "warn". An unrecognised value reports false so a composition root
// keeps the level it already has instead of resetting verbosity mid-incident.
func TestParseLogLevel(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  slog.Level
		wantK bool
	}{
		{"debug", slog.LevelDebug, true},
		{"DEBUG", slog.LevelDebug, true},
		{" Info ", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"", slog.LevelInfo, false},
		{"verbose", slog.LevelInfo, false},
		{"trace", slog.LevelInfo, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ports.ParseLogLevel(tc.in)
			if ok != tc.wantK {
				t.Fatalf("ParseLogLevel(%q) ok = %v, want %v", tc.in, ok, tc.wantK)
			}
			if ok && got != tc.want {
				t.Fatalf("ParseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestLogLevelNames_ParseEveryAdvertisedName keeps the documented enum and the
// parser from drifting apart: every name the validator offers an operator must
// be a name the runtime can actually apply.
func TestLogLevelNames_ParseEveryAdvertisedName(t *testing.T) {
	names := ports.LogLevelNames()
	if len(names) == 0 {
		t.Fatal("LogLevelNames must advertise the enum")
	}
	if !slices.IsSorted(names) {
		t.Fatalf("LogLevelNames must be sorted for stable messages and docs, got %v", names)
	}
	for _, name := range names {
		if _, ok := ports.ParseLogLevel(name); !ok {
			t.Fatalf("advertised log level %q is not parseable", name)
		}
	}
}

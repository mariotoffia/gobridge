package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bridge.log_level is a closed enum, but nothing validated it: a misspelled
// level was accepted by the config transaction and then silently ignored by the
// bootstrap (which keeps the current level for an unrecognised value), so an
// operator who set "DEUBG" to diagnose an incident saw a successful commit and
// no extra logging. Validation must reject exactly what the parser cannot use.

func TestValidate_LogLevelUnknownRejected(t *testing.T) {
	for _, level := range []string{"deubg", "verbose", "trace", "fatal", "12"} {
		t.Run(level, func(t *testing.T) {
			cfg := minimalValidConfig("log-level-bridge")
			cfg.Bridge.LogLevel = level
			err := Validate(cfg)
			require.Error(t, err, "an unknown log level must not commit and then be ignored")
			assert.Contains(t, err.Error(), "bridge.log_level")
		})
	}
}

// TestValidate_LogLevelAcceptedValues is the negative control: every spelling
// the parser honours — including the "warning" alias and mixed case, which it
// lower-cases and trims — must pass validation. Rejecting a value the runtime
// would happily apply is as much a defect as accepting one it ignores.
func TestValidate_LogLevelAcceptedValues(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "warning", "error", "WARN", " info "} {
		t.Run("level="+level, func(t *testing.T) {
			cfg := minimalValidConfig("log-level-bridge")
			cfg.Bridge.LogLevel = level
			require.NoError(t, Validate(cfg), "the runtime applies %q, so validation must accept it", level)
		})
	}
}

package otelmetrics

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// MF-5: export failures must not be silent by default — an unset error
// handler falls back to slog warn logging.
func TestApplyDefaults_ErrorHandlerDefaultsToWarnLogger(t *testing.T) {
	t.Parallel()

	cfg := Config{}
	applyDefaults(&cfg)
	require.NotNil(t, cfg.errorHandler, "unset handler must default to slog warn logging")
	// The default handler must be safe to invoke.
	cfg.errorHandler(errors.New("synthetic export failure"))
}

// MF-5: WithErrorHandler(nil) is an explicit opt-out — the default warn
// logger must not be installed over it.
func TestWithErrorHandler_NilIsExplicitOptOut(t *testing.T) {
	t.Parallel()

	e := NewForTest(WithErrorHandler(nil))
	assert.Nil(t, e.config.errorHandler, "explicit nil handler must suppress reporting")
	assert.True(t, e.config.errorHandlerSet)
}

// MF-8: a caller that already stamped instance_id via WithDefaultTags
// must not get a duplicate tag from WithInstanceTag.
func TestApplyDefaults_InstanceTagIdempotent(t *testing.T) {
	t.Parallel()

	cfg := Config{
		InstanceID:  "bridge-1",
		DefaultTags: []shared.Tag{{Key: TagKeyInstanceID, Value: "manual"}},
	}
	applyDefaults(&cfg)

	count := 0
	for _, tag := range cfg.DefaultTags {
		if tag.Key == TagKeyInstanceID {
			count++
		}
	}
	assert.Equal(t, 1, count, "instance_id must not be duplicated")
	assert.Equal(t, "manual", cfg.DefaultTags[0].Value, "explicit default tag wins")
}

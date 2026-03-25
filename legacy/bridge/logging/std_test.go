package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSlogFactory_CreatesComponentBoundLogCreator(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	base := slog.New(handler)

	factory := logging.NewSlogFactory(base)
	bridgeLog := factory("bridge")

	bridgeLog(context.Background(), types.LogLevelInfo).
		Str("topic", "sensor/temp").
		Msg("publishing")

	out := buf.String()
	assert.Contains(t, out, "publishing", "expected message in log")
	assert.Contains(t, out, "component=bridge", "expected component attr")
	assert.Contains(t, out, "topic=sensor/temp", "expected topic attr")
}

func TestNewSlogFactory_DifferentComponentsHaveOwnLogCreators(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	base := slog.New(handler)

	factory := logging.NewSlogFactory(base)
	bridgeLog := factory("bridge")
	pipelineLog := factory("pipeline:test")

	bridgeLog(context.Background(), types.LogLevelInfo).Msg("bridge message")
	pipelineLog(context.Background(), types.LogLevelInfo).Msg("pipeline message")

	out := buf.String()
	assert.Contains(t, out, "component=bridge", "expected bridge component")
	// slog text handler may or may not quote the value depending on content
	assert.True(t, strings.Contains(out, "component=pipeline:test") || strings.Contains(out, "component=\"pipeline:test\""), "expected pipeline component")
}

func TestStandardLogger_JSONOutput(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, nil)
	base := slog.New(handler)

	factory := logging.NewSlogFactory(base)
	log := factory("bridge")

	log(context.Background(), types.LogLevelInfo).
		AsJSON("json", struct {
			A string
			B int
		}{A: "universe", B: 42}).
		Str("topic", "sensor/temp").
		Msg("publishing")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, buf.String())
	}

	assert.Equal(t, "publishing", entry["msg"])
	assert.Equal(t, "bridge", entry["component"])
	assert.Equal(t, "sensor/temp", entry["topic"])

	jsonField, ok := entry["json"].(map[string]any)
	require.True(t, ok, "expected json field to be a map, got %T", entry["json"])

	assert.Equal(t, "universe", jsonField["A"])
	assert.Equal(t, float64(42), jsonField["B"]) // JSON numbers are float64
}

func TestStandardLogger_AllMethods(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, nil)
	base := slog.New(handler)

	factory := logging.NewSlogFactory(base)
	log := factory("test")

	log(context.Background(), types.LogLevelInfo).
		Str("str_key", "str_value").
		Int("int_key", 42).
		Bool("bool_key", true).
		Err(assert.AnError).
		AsJSON("obj", map[string]string{"nested": "value"}).
		Msg("test message")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))

	assert.Equal(t, "test", entry["component"])
	assert.Equal(t, "str_value", entry["str_key"])
	assert.Equal(t, float64(42), entry["int_key"])
	assert.Equal(t, true, entry["bool_key"])
	assert.NotNil(t, entry["error"])
}

func TestStandardLogger_Msgf(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	base := slog.New(handler)

	factory := logging.NewSlogFactory(base)
	log := factory("test")

	log(context.Background(), types.LogLevelInfo).
		Str("key", "value").
		Msgf("formatted %s %d", "message", 42)

	out := buf.String()
	assert.Contains(t, out, "formatted message 42")
	assert.Contains(t, out, "key=value")
}

func TestStandardLogger_LogLevels(t *testing.T) {
	testCases := []struct {
		level    types.LogLevel
		expected string
	}{
		{types.LogLevelDebug, "DEBUG"},
		{types.LogLevelInfo, "INFO"},
		{types.LogLevelWarn, "WARN"},
		{types.LogLevelError, "ERROR"},
	}

	for _, tc := range testCases {
		t.Run(tc.expected, func(t *testing.T) {
			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
			base := slog.New(handler)

			factory := logging.NewSlogFactory(base)
			log := factory("test")

			log(context.Background(), tc.level).Msg("test")

			out := buf.String()
			assert.Contains(t, out, tc.expected, "expected log level in output")
		})
	}
}

func TestStandardLogger_ErrNilDoesNotAddAttribute(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, nil)
	base := slog.New(handler)

	factory := logging.NewSlogFactory(base)
	log := factory("test")

	log(context.Background(), types.LogLevelInfo).
		Err(nil).
		Msg("no error")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))

	_, hasError := entry["error"]
	assert.False(t, hasError, "should not have error attribute when err is nil")
}

func TestNewSlogCreator_DeprecatedButWorks(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	base := slog.New(handler)

	// Test that deprecated NewSlogCreator still works
	creator := logging.NewSlogCreator(base)
	logger := creator(context.Background(), types.LogLevelInfo).
		Str("topic", "sensor/temp")

	logger.Msg("publishing")

	out := buf.String()
	assert.True(t, strings.Contains(out, "publishing"), "expected message in log")
	assert.True(t, strings.Contains(out, "topic=sensor/temp"), "expected topic attr")
	// Component should not be present since NewSlogCreator doesn't set one
	assert.False(t, strings.Contains(out, "component="), "should not have component attr")
}

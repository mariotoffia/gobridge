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

func TestStandardLogger_LogToBuffer(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	base := slog.New(handler)

	creator := logging.NewSlogCreator(base)
	logger := creator(context.Background(), types.LogLevelInfo).
		WithService("bridge").
		WithMethod("publish").
		Str("topic", "sensor/temp")

	logger.Msg("publishing")

	out := buf.String()
	if !strings.Contains(out, "publishing") {
		t.Errorf("expected message in log, got: %s", out)
	}
	if !strings.Contains(out, "service=bridge") {
		t.Errorf("expected service attr, got: %s", out)
	}
	if !strings.Contains(out, "method=publish") {
		t.Errorf("expected method attr, got: %s", out)
	}
}

func TestStandardLogger_JSONOutput(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, nil)
	base := slog.New(handler)

	creator := logging.NewSlogCreator(base)
	logger := creator(context.Background(), types.LogLevelInfo).
		WithService("bridge").
		WithMethod("publish").
		AsJSON("json", struct {
			A string
			B int
		}{A: "universe", B: 42}).
		Str("topic", "sensor/temp")

	logger.Msg("publishing")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, buf.String())
	}

	assert.Equal(t, entry["msg"], "publishing")
	assert.Equal(t, entry["service"], "bridge")
	assert.Equal(t, entry["method"], "publish")
	assert.Equal(t, entry["topic"], "sensor/temp")

	jsonField, ok := entry["json"].(map[string]any)
	require.True(t, ok, "expected json field to be a map, got %T", entry["json"])

	assert.Equal(t, jsonField["A"], "universe")
	assert.Equal(t, jsonField["B"], float64(42)) // JSON numbers are float64
}

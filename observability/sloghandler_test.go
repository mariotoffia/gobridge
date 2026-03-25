package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/mariotoffia/gobridge/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	inner := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(observability.NewCorrelationHandler(inner))
}

func parseJSON(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	return m
}

func TestCorrelationHandler_AllFields(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	ctx := context.Background()
	ctx = observability.WithCorrelationID(ctx, "corr-001")
	ctx = observability.WithTraceID(ctx, "trace-abc")
	ctx = observability.WithSpanID(ctx, "span-xyz")

	logger.InfoContext(ctx, "all fields present")

	m := parseJSON(t, &buf)
	assert.Equal(t, "corr-001", m["correlation_id"])
	assert.Equal(t, "trace-abc", m["trace_id"])
	assert.Equal(t, "span-xyz", m["span_id"])
	assert.Equal(t, "all fields present", m["msg"])
}

func TestCorrelationHandler_PartialFields(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	ctx := observability.WithCorrelationID(context.Background(), "corr-only")

	logger.InfoContext(ctx, "partial")

	m := parseJSON(t, &buf)
	assert.Equal(t, "corr-only", m["correlation_id"])
	assert.NotContains(t, m, "trace_id")
	assert.NotContains(t, m, "span_id")
}

func TestCorrelationHandler_NoFields(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	logger.InfoContext(context.Background(), "no ids")

	m := parseJSON(t, &buf)
	assert.NotContains(t, m, "correlation_id")
	assert.NotContains(t, m, "trace_id")
	assert.NotContains(t, m, "span_id")
	assert.Equal(t, "no ids", m["msg"])
}

func TestCorrelationHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := observability.NewCorrelationHandler(inner).
		WithAttrs([]slog.Attr{slog.String("service", "test-svc")})
	logger := slog.New(handler)

	ctx := observability.WithCorrelationID(context.Background(), "corr-attr")

	logger.InfoContext(ctx, "with attrs")

	m := parseJSON(t, &buf)
	assert.Equal(t, "corr-attr", m["correlation_id"])
	assert.Equal(t, "test-svc", m["service"])
}

func TestCorrelationHandler_WithGroup(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := observability.NewCorrelationHandler(inner).
		WithGroup("request")
	logger := slog.New(handler)

	ctx := observability.WithCorrelationID(context.Background(), "corr-grp")

	logger.InfoContext(ctx, "grouped", slog.String("path", "/api"))

	m := parseJSON(t, &buf)
	assert.Equal(t, "grouped", m["msg"])

	group, ok := m["request"].(map[string]any)
	require.True(t, ok, "expected 'request' group in output")
	assert.Equal(t, "/api", group["path"])
	assert.Equal(t, "corr-grp", group["correlation_id"])
}

func TestCorrelationHandler_Enabled(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	handler := observability.NewCorrelationHandler(inner)

	assert.False(t, handler.Enabled(context.Background(), slog.LevelInfo))
	assert.True(t, handler.Enabled(context.Background(), slog.LevelWarn))
	assert.True(t, handler.Enabled(context.Background(), slog.LevelError))
}

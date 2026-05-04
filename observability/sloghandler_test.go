package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

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

// Verifies correlation_id, trace_id, and span_id are emitted when all are present on the log context.
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

// Verifies only correlation_id appears when trace and span IDs are not set on the context.
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

// Verifies no correlation or trace fields are added when the context carries no IDs.
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

// Verifies WithAttrs on the handler still merges correlation_id from context with fixed handler attributes.
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

// Verifies correlation_id is placed inside a slog group when the handler uses WithGroup.
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

// TestNewCorrelationHandler_NilInner_Panics validates that passing nil
// to NewCorrelationHandler panics with the expected message.
func TestNewCorrelationHandler_NilInner_Panics(t *testing.T) {
	require.PanicsWithValue(t,
		"observability: inner handler must not be nil",
		func() { observability.NewCorrelationHandler(nil) },
	)
}

// failingHandler is a slog.Handler stub that always returns an error from Handle.
type failingHandler struct{}

func (failingHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (failingHandler) Handle(context.Context, slog.Record) error { return errWriteFailed }
func (failingHandler) WithAttrs([]slog.Attr) slog.Handler        { return failingHandler{} }
func (failingHandler) WithGroup(string) slog.Handler             { return failingHandler{} }

var errWriteFailed = errors.New("write failed")

// TestCorrelationHandler_Handle_PropagatesInnerError validates that when
// the inner handler returns an error, Handle propagates it unchanged.
func TestCorrelationHandler_Handle_PropagatesInnerError(t *testing.T) {
	h := observability.NewCorrelationHandler(failingHandler{})

	ctx := observability.WithCorrelationID(context.Background(), "corr-err")
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "test", 0)

	err := h.Handle(ctx, rec)
	require.ErrorIs(t, err, errWriteFailed)
}

// Verifies Enabled reflects the wrapped handler's minimum level.
func TestCorrelationHandler_Enabled(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	handler := observability.NewCorrelationHandler(inner)

	assert.False(t, handler.Enabled(context.Background(), slog.LevelInfo))
	assert.True(t, handler.Enabled(context.Background(), slog.LevelWarn))
	assert.True(t, handler.Enabled(context.Background(), slog.LevelError))
}

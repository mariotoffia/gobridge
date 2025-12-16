package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// ContextLogger Tests
//
// Validates the ContextLogger wrapper that automatically injects
// correlation and trace IDs from context into log entries.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                     ContextLogger Architecture                          │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  Context ──▶ ┌────────────────────┐ ──▶ Logger                          │
// │              │   ContextLogger    │                                      │
// │              │  ┌──────────────┐  │                                      │
// │              │  │ correlationId│  │                                      │
// │              │  │ traceId      │  │                                      │
// │              │  │ spanId       │  │ ──withIDs()──▶ enriched logger      │
// │              │  └──────────────┘  │                                      │
// │              └────────────────────┘                                      │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// testLogger creates a test logger that writes JSON to a buffer.
func testLogger(buf *bytes.Buffer) types.Logger {
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	base := slog.New(handler)
	creator := logging.NewSlogCreator(base)
	return creator(context.Background(), types.LogLevelDebug)
}

// parseLogEntry parses a JSON log entry from the buffer.
func parseLogEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log entry: %v\nraw: %s", err, buf.String())
	}
	return entry
}

// ═══════════════════════════════════════════════════════════════════════════
// Creation Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestNewContextLogger validates context logger creation.
func TestNewContextLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)
	ctx := context.Background()

	cl := NewContextLogger(ctx, logger)

	if cl == nil {
		t.Fatal("NewContextLogger() returned nil")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// ID Injection Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestContextLogger_Msg_WithIDs validates that Msg injects context IDs.
func TestContextLogger_Msg_WithIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := context.Background()
	ctx = WithCorrelationID(ctx, "test-corr-id")
	ctx = WithTraceID(ctx, "test-trace-id")
	ctx = WithSpanID(ctx, "test-span-id")

	cl := NewContextLogger(ctx, logger)
	cl.Msg("test message")

	entry := parseLogEntry(t, &buf)

	if entry["correlationId"] != "test-corr-id" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "test-corr-id")
	}
	if entry["traceId"] != "test-trace-id" {
		t.Errorf("traceId = %v, want %q", entry["traceId"], "test-trace-id")
	}
	if entry["spanId"] != "test-span-id" {
		t.Errorf("spanId = %v, want %q", entry["spanId"], "test-span-id")
	}
	if entry["msg"] != "test message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "test message")
	}
}

// TestContextLogger_Msg_NoIDs validates Msg with empty context.
func TestContextLogger_Msg_NoIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	cl := NewContextLogger(context.Background(), logger)
	cl.Msg("no ids message")

	entry := parseLogEntry(t, &buf)

	// IDs should not be present when not set
	if _, exists := entry["correlationId"]; exists {
		t.Errorf("correlationId should not be present, got %v", entry["correlationId"])
	}
	if _, exists := entry["traceId"]; exists {
		t.Errorf("traceId should not be present, got %v", entry["traceId"])
	}
	if _, exists := entry["spanId"]; exists {
		t.Errorf("spanId should not be present, got %v", entry["spanId"])
	}
}

// TestContextLogger_Msgf_WithIDs validates formatted message with IDs.
func TestContextLogger_Msgf_WithIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "msgf-corr")

	cl := NewContextLogger(ctx, logger)
	cl.Msgf("formatted %s %d", "message", 42)

	entry := parseLogEntry(t, &buf)

	if entry["correlationId"] != "msgf-corr" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "msgf-corr")
	}
	if entry["msg"] != "formatted message 42" {
		t.Errorf("msg = %v, want %q", entry["msg"], "formatted message 42")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Field Builder Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestContextLogger_WithMethod validates method field addition using Str.
func TestContextLogger_WithMethod(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "method-corr")
	cl := NewContextLogger(ctx, logger)

	// Use Str("method", ...) instead of removed WithMethod
	cl.Str("method", "TestMethod").Msg("with method")

	entry := parseLogEntry(t, &buf)

	if entry["method"] != "TestMethod" {
		t.Errorf("method = %v, want %q", entry["method"], "TestMethod")
	}
	if entry["correlationId"] != "method-corr" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "method-corr")
	}
}

// TestContextLogger_WithService validates service field addition using Str.
func TestContextLogger_WithService(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "service-corr")
	cl := NewContextLogger(ctx, logger)

	// Use Str("service", ...) instead of removed WithService
	cl.Str("service", "TestService").Msg("with service")

	entry := parseLogEntry(t, &buf)

	if entry["service"] != "TestService" {
		t.Errorf("service = %v, want %q", entry["service"], "TestService")
	}
}

// TestContextLogger_Str validates string field addition.
func TestContextLogger_Str(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	cl := NewContextLogger(context.Background(), logger)
	cl.Str("key", "value").Msg("with string")

	entry := parseLogEntry(t, &buf)

	if entry["key"] != "value" {
		t.Errorf("key = %v, want %q", entry["key"], "value")
	}
}

// TestContextLogger_Int validates integer field addition.
func TestContextLogger_Int(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	cl := NewContextLogger(context.Background(), logger)
	cl.Int("count", 42).Msg("with int")

	entry := parseLogEntry(t, &buf)

	if entry["count"] != float64(42) { // JSON numbers are float64
		t.Errorf("count = %v, want %v", entry["count"], 42)
	}
}

// TestContextLogger_Bool validates boolean field addition.
func TestContextLogger_Bool(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	cl := NewContextLogger(context.Background(), logger)
	cl.Bool("enabled", true).Msg("with bool")

	entry := parseLogEntry(t, &buf)

	if entry["enabled"] != true {
		t.Errorf("enabled = %v, want %v", entry["enabled"], true)
	}
}

// TestContextLogger_Err validates error field addition.
func TestContextLogger_Err(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	cl := NewContextLogger(context.Background(), logger)
	cl.Err(types.ErrNotFound).Msg("with error")

	entry := parseLogEntry(t, &buf)

	// Error should be logged (exact format depends on logger implementation)
	if _, exists := entry["error"]; !exists {
		t.Error("error field should be present")
	}
}

// TestContextLogger_AsJSON validates JSON field addition.
func TestContextLogger_AsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	data := struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}{Name: "test", Count: 5}

	cl := NewContextLogger(context.Background(), logger)
	cl.AsJSON("data", data).Msg("with json")

	entry := parseLogEntry(t, &buf)

	jsonData, ok := entry["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %T, want map[string]any", entry["data"])
	}
	if jsonData["name"] != "test" {
		t.Errorf("data.name = %v, want %q", jsonData["name"], "test")
	}
}

// TestContextLogger_Chaining validates method chaining preserves context.
func TestContextLogger_Chaining(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "chain-corr")
	ctx = WithTraceID(ctx, "chain-trace")

	cl := NewContextLogger(ctx, logger)
	// Use Str("service", ...) and Str("method", ...) instead of removed methods
	cl.Str("service", "ChainService").
		Str("method", "ChainMethod").
		Str("key", "value").
		Int("num", 123).
		Bool("flag", true).
		Msg("chained message")

	entry := parseLogEntry(t, &buf)

	// All fields should be present
	if entry["correlationId"] != "chain-corr" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "chain-corr")
	}
	if entry["traceId"] != "chain-trace" {
		t.Errorf("traceId = %v, want %q", entry["traceId"], "chain-trace")
	}
	if entry["service"] != "ChainService" {
		t.Errorf("service = %v, want %q", entry["service"], "ChainService")
	}
	if entry["method"] != "ChainMethod" {
		t.Errorf("method = %v, want %q", entry["method"], "ChainMethod")
	}
	if entry["key"] != "value" {
		t.Errorf("key = %v, want %q", entry["key"], "value")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Factory Function Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestLoggerFromContext validates the factory function.
func TestLoggerFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "factory-corr")

	cl := LoggerFromContext(ctx, logger)
	cl.Msg("from factory")

	entry := parseLogEntry(t, &buf)

	if entry["correlationId"] != "factory-corr" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "factory-corr")
	}
}

// TestLoggerWithIDs validates creating logger with explicit IDs.
func TestLoggerWithIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	lc := LogContext{
		CorrelationID: "explicit-corr",
		TraceID:       "explicit-trace",
		SpanID:        "explicit-span",
	}

	enrichedLogger := LoggerWithIDs(logger, lc)
	enrichedLogger.Msg("with explicit ids")

	entry := parseLogEntry(t, &buf)

	if entry["correlationId"] != "explicit-corr" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "explicit-corr")
	}
	if entry["traceId"] != "explicit-trace" {
		t.Errorf("traceId = %v, want %q", entry["traceId"], "explicit-trace")
	}
	if entry["spanId"] != "explicit-span" {
		t.Errorf("spanId = %v, want %q", entry["spanId"], "explicit-span")
	}
}

// TestLoggerWithIDs_Partial validates partial ID injection.
func TestLoggerWithIDs_Partial(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	lc := LogContext{
		CorrelationID: "only-corr",
		// TraceID and SpanID empty
	}

	enrichedLogger := LoggerWithIDs(logger, lc)
	enrichedLogger.Msg("partial ids")

	entry := parseLogEntry(t, &buf)

	if entry["correlationId"] != "only-corr" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "only-corr")
	}
	// Empty IDs should not be injected
	if _, exists := entry["traceId"]; exists {
		t.Errorf("traceId should not be present, got %v", entry["traceId"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Convenience Function Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestDebug validates the Debug convenience function.
func TestDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "debug-corr")

	Debug(ctx, logger, "debug message")

	entry := parseLogEntry(t, &buf)

	if entry["msg"] != "debug message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "debug message")
	}
	if entry["correlationId"] != "debug-corr" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "debug-corr")
	}
}

// TestInfo validates the Info convenience function.
func TestInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "info-corr")

	Info(ctx, logger, "info message")

	entry := parseLogEntry(t, &buf)

	if entry["msg"] != "info message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "info message")
	}
}

// TestWarn validates the Warn convenience function.
func TestWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "warn-corr")

	Warn(ctx, logger, "warn message")

	entry := parseLogEntry(t, &buf)

	if entry["msg"] != "warn message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "warn message")
	}
}

// TestErrorLog validates the ErrorLog convenience function.
func TestErrorLog(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx := WithCorrelationID(context.Background(), "error-corr")

	ErrorLog(ctx, logger, types.ErrNotFound, "error message")

	entry := parseLogEntry(t, &buf)

	if entry["msg"] != "error message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "error message")
	}
	if entry["correlationId"] != "error-corr" {
		t.Errorf("correlationId = %v, want %q", entry["correlationId"], "error-corr")
	}
	// Error should be present
	if _, exists := entry["error"]; !exists {
		t.Error("error field should be present")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Interface Compliance Test
// ═══════════════════════════════════════════════════════════════════════════

// TestContextLogger_ImplementsLogger validates interface compliance.
func TestContextLogger_ImplementsLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	var _ types.Logger = NewContextLogger(context.Background(), logger)
}

package logging

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// Correlation ID Unit Tests
//
// Validates correlation ID generation, context handling, and metadata injection.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                   Correlation ID Flow                                    │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  Context ──▶ [Extract] ──▶ ID found? ──yes──▶ use ID                    │
// │     │           │                                                        │
// │     │           └──no──▶ Message Metadata ──▶ ID found? ──yes──▶ use ID │
// │     │                          │                                         │
// │     │                          └──no──▶ [Generate] ──▶ new ID           │
// │     │                                                                    │
// │     └──▶ [Inject] ──▶ Message Metadata                                  │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestGenerateCorrelationID validates ID generation.
func TestGenerateCorrelationID(t *testing.T) {
	id1 := GenerateCorrelationID()
	id2 := GenerateCorrelationID()

	if id1 == "" {
		t.Error("expected non-empty correlation ID")
	}
	if id1 == id2 {
		t.Error("expected unique correlation IDs")
	}

	// Should be 32 characters (16 bytes hex encoded)
	if len(id1) != 32 {
		t.Errorf("expected 32 character ID, got %d", len(id1))
	}
}

// TestWithCorrelationID validates context handling.
func TestWithCorrelationID(t *testing.T) {
	ctx := context.Background()

	// Initially empty
	if id := GetCorrelationID(ctx); id != "" {
		t.Errorf("expected empty ID, got %s", id)
	}

	// Set ID
	ctx = WithCorrelationID(ctx, "test-id-123")

	id := GetCorrelationID(ctx)
	if id != "test-id-123" {
		t.Errorf("expected test-id-123, got %s", id)
	}
}

// TestExtractOrGenerateCorrelationID validates extraction from different sources.
func TestExtractOrGenerateCorrelationID(t *testing.T) {
	// Test 1: Extract from context
	ctx := WithCorrelationID(context.Background(), "from-context")
	msg := &types.Message{}

	newCtx, id := ExtractOrGenerateCorrelationID(ctx, msg)
	if id != "from-context" {
		t.Errorf("expected from-context, got %s", id)
	}
	if GetCorrelationID(newCtx) != "from-context" {
		t.Error("expected context to have correlation ID")
	}

	// Test 2: Extract from message metadata
	ctx = context.Background()
	msg = &types.Message{
		Metadata: map[string]any{
			CorrelationIDMetadataKey: "from-metadata",
		},
	}

	newCtx, id = ExtractOrGenerateCorrelationID(ctx, msg)
	if id != "from-metadata" {
		t.Errorf("expected from-metadata, got %s", id)
	}
	if GetCorrelationID(newCtx) != "from-metadata" {
		t.Error("expected context to have correlation ID from metadata")
	}

	// Test 3: Generate new when not present
	ctx = context.Background()
	msg = &types.Message{}

	newCtx, id = ExtractOrGenerateCorrelationID(ctx, msg)
	if id == "" {
		t.Error("expected generated correlation ID")
	}
	if GetCorrelationID(newCtx) != id {
		t.Error("expected context to have generated correlation ID")
	}
}

// TestInjectCorrelationID validates metadata injection.
func TestInjectCorrelationID(t *testing.T) {
	// Test with nil metadata
	msg := &types.Message{}
	InjectCorrelationID(msg, "injected-id")

	if msg.Metadata == nil {
		t.Fatal("expected metadata to be initialized")
	}
	if msg.Metadata[CorrelationIDMetadataKey] != "injected-id" {
		t.Errorf("expected injected-id, got %v", msg.Metadata[CorrelationIDMetadataKey])
	}

	// Test with existing metadata
	msg = &types.Message{
		Metadata: map[string]any{
			"existing": "value",
		},
	}
	InjectCorrelationID(msg, "new-id")

	if msg.Metadata["existing"] != "value" {
		t.Error("expected existing metadata to be preserved")
	}
	if msg.Metadata[CorrelationIDMetadataKey] != "new-id" {
		t.Errorf("expected new-id, got %v", msg.Metadata[CorrelationIDMetadataKey])
	}
}

// TestCorrelationIDConstants validates constants.
func TestCorrelationIDConstants(t *testing.T) {
	if CorrelationIDMetadataKey != "_correlationId" {
		t.Errorf("unexpected metadata key: %s", CorrelationIDMetadataKey)
	}
	if CorrelationIDHeaderKey != "X-Correlation-ID" {
		t.Errorf("unexpected header key: %s", CorrelationIDHeaderKey)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Trace ID Unit Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestGenerateTraceID validates trace ID generation.
//
// Trace ID: 32 hex characters (16 bytes)
func TestGenerateTraceID(t *testing.T) {
	id1 := GenerateTraceID()
	id2 := GenerateTraceID()

	if id1 == "" {
		t.Error("expected non-empty trace ID")
	}
	if id1 == id2 {
		t.Error("expected unique trace IDs")
	}

	// Should be 32 characters (16 bytes hex encoded)
	if len(id1) != 32 {
		t.Errorf("expected 32 character trace ID, got %d", len(id1))
	}
}

// TestGenerateSpanID validates span ID generation.
//
// Span ID: 16 hex characters (8 bytes)
func TestGenerateSpanID(t *testing.T) {
	id1 := GenerateSpanID()
	id2 := GenerateSpanID()

	if id1 == "" {
		t.Error("expected non-empty span ID")
	}
	if id1 == id2 {
		t.Error("expected unique span IDs")
	}

	// Should be 16 characters (8 bytes hex encoded)
	if len(id1) != 16 {
		t.Errorf("expected 16 character span ID, got %d", len(id1))
	}
}

// TestWithTraceID_GetTraceID validates trace ID context handling.
func TestWithTraceID_GetTraceID(t *testing.T) {
	ctx := context.Background()

	// Initially empty
	if id := GetTraceID(ctx); id != "" {
		t.Errorf("expected empty trace ID, got %s", id)
	}

	// Set ID
	ctx = WithTraceID(ctx, "trace-abc123")

	if id := GetTraceID(ctx); id != "trace-abc123" {
		t.Errorf("expected trace-abc123, got %s", id)
	}
}

// TestWithSpanID_GetSpanID validates span ID context handling.
func TestWithSpanID_GetSpanID(t *testing.T) {
	ctx := context.Background()

	// Initially empty
	if id := GetSpanID(ctx); id != "" {
		t.Errorf("expected empty span ID, got %s", id)
	}

	// Set ID
	ctx = WithSpanID(ctx, "span-xyz789")

	if id := GetSpanID(ctx); id != "span-xyz789" {
		t.Errorf("expected span-xyz789, got %s", id)
	}
}

// TestExtractOrGenerateTraceID validates trace ID extraction from different sources.
func TestExtractOrGenerateTraceID(t *testing.T) {
	t.Run("from context", func(t *testing.T) {
		ctx := WithTraceID(context.Background(), "ctx-trace-id")
		msg := &types.Message{}

		newCtx, id := ExtractOrGenerateTraceID(ctx, msg)
		if id != "ctx-trace-id" {
			t.Errorf("expected ctx-trace-id, got %s", id)
		}
		if GetTraceID(newCtx) != "ctx-trace-id" {
			t.Error("expected context to have trace ID")
		}
	})

	t.Run("from metadata", func(t *testing.T) {
		ctx := context.Background()
		msg := &types.Message{
			Metadata: map[string]any{
				TraceIDMetadataKey: "metadata-trace-id",
			},
		}

		newCtx, id := ExtractOrGenerateTraceID(ctx, msg)
		if id != "metadata-trace-id" {
			t.Errorf("expected metadata-trace-id, got %s", id)
		}
		if GetTraceID(newCtx) != "metadata-trace-id" {
			t.Error("expected context to have trace ID from metadata")
		}
	})

	t.Run("generate new", func(t *testing.T) {
		ctx := context.Background()
		msg := &types.Message{}

		newCtx, id := ExtractOrGenerateTraceID(ctx, msg)
		if id == "" {
			t.Error("expected generated trace ID")
		}
		if len(id) != 32 {
			t.Errorf("expected 32 character trace ID, got %d", len(id))
		}
		if GetTraceID(newCtx) != id {
			t.Error("expected context to have generated trace ID")
		}
	})
}

// TestInjectTraceID validates trace ID metadata injection.
func TestInjectTraceID(t *testing.T) {
	msg := &types.Message{}
	InjectTraceID(msg, "injected-trace")

	if msg.Metadata == nil {
		t.Fatal("expected metadata to be initialized")
	}
	if msg.Metadata[TraceIDMetadataKey] != "injected-trace" {
		t.Errorf("expected injected-trace, got %v", msg.Metadata[TraceIDMetadataKey])
	}
}

// TestInjectSpanID validates span ID metadata injection.
func TestInjectSpanID(t *testing.T) {
	msg := &types.Message{}
	InjectSpanID(msg, "injected-span")

	if msg.Metadata == nil {
		t.Fatal("expected metadata to be initialized")
	}
	if msg.Metadata[SpanIDMetadataKey] != "injected-span" {
		t.Errorf("expected injected-span, got %v", msg.Metadata[SpanIDMetadataKey])
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// LogContext Tests
//
// Validates the LogContext struct and combined context operations.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │                        LogContext Structure                              │
// ├─────────────────────────────────────────────────────────────────────────┤
// │  ┌───────────────────────────────────────────────────────────────────┐  │
// │  │ LogContext                                                         │  │
// │  │   CorrelationID: string                                           │  │
// │  │   TraceID: string                                                  │  │
// │  │   SpanID: string                                                   │  │
// │  └───────────────────────────────────────────────────────────────────┘  │
// └─────────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestLogContext_Struct validates LogContext field population.
func TestLogContext_Struct(t *testing.T) {
	lc := LogContext{
		CorrelationID: "corr-123",
		TraceID:       "trace-456",
		SpanID:        "span-789",
	}

	if lc.CorrelationID != "corr-123" {
		t.Errorf("CorrelationID = %q, want %q", lc.CorrelationID, "corr-123")
	}
	if lc.TraceID != "trace-456" {
		t.Errorf("TraceID = %q, want %q", lc.TraceID, "trace-456")
	}
	if lc.SpanID != "span-789" {
		t.Errorf("SpanID = %q, want %q", lc.SpanID, "span-789")
	}
}

// TestGetLogContext validates extracting all IDs from context.
func TestGetLogContext(t *testing.T) {
	ctx := context.Background()
	ctx = WithCorrelationID(ctx, "corr-id")
	ctx = WithTraceID(ctx, "trace-id")
	ctx = WithSpanID(ctx, "span-id")

	lc := GetLogContext(ctx)

	if lc.CorrelationID != "corr-id" {
		t.Errorf("CorrelationID = %q, want %q", lc.CorrelationID, "corr-id")
	}
	if lc.TraceID != "trace-id" {
		t.Errorf("TraceID = %q, want %q", lc.TraceID, "trace-id")
	}
	if lc.SpanID != "span-id" {
		t.Errorf("SpanID = %q, want %q", lc.SpanID, "span-id")
	}
}

// TestGetLogContext_Empty validates GetLogContext with empty context.
func TestGetLogContext_Empty(t *testing.T) {
	ctx := context.Background()
	lc := GetLogContext(ctx)

	if lc.CorrelationID != "" {
		t.Errorf("CorrelationID = %q, want empty", lc.CorrelationID)
	}
	if lc.TraceID != "" {
		t.Errorf("TraceID = %q, want empty", lc.TraceID)
	}
	if lc.SpanID != "" {
		t.Errorf("SpanID = %q, want empty", lc.SpanID)
	}
}

// TestWithLogContext validates setting all IDs in context.
func TestWithLogContext(t *testing.T) {
	lc := LogContext{
		CorrelationID: "with-corr",
		TraceID:       "with-trace",
		SpanID:        "with-span",
	}

	ctx := WithLogContext(context.Background(), lc)

	if GetCorrelationID(ctx) != "with-corr" {
		t.Errorf("GetCorrelationID() = %q, want %q", GetCorrelationID(ctx), "with-corr")
	}
	if GetTraceID(ctx) != "with-trace" {
		t.Errorf("GetTraceID() = %q, want %q", GetTraceID(ctx), "with-trace")
	}
	if GetSpanID(ctx) != "with-span" {
		t.Errorf("GetSpanID() = %q, want %q", GetSpanID(ctx), "with-span")
	}
}

// TestWithLogContext_Partial validates setting partial IDs in context.
func TestWithLogContext_Partial(t *testing.T) {
	// Only set correlation ID
	lc := LogContext{
		CorrelationID: "only-corr",
	}

	ctx := WithLogContext(context.Background(), lc)

	if GetCorrelationID(ctx) != "only-corr" {
		t.Errorf("GetCorrelationID() = %q, want %q", GetCorrelationID(ctx), "only-corr")
	}
	// Empty IDs should not be set
	if GetTraceID(ctx) != "" {
		t.Errorf("GetTraceID() = %q, want empty", GetTraceID(ctx))
	}
	if GetSpanID(ctx) != "" {
		t.Errorf("GetSpanID() = %q, want empty", GetSpanID(ctx))
	}
}

// TestExtractOrGenerateLogContext validates full extraction/generation.
func TestExtractOrGenerateLogContext(t *testing.T) {
	t.Run("all from context", func(t *testing.T) {
		ctx := context.Background()
		ctx = WithCorrelationID(ctx, "ctx-corr")
		ctx = WithTraceID(ctx, "ctx-trace")
		ctx = WithSpanID(ctx, "ctx-span")

		msg := &types.Message{}
		newCtx, lc := ExtractOrGenerateLogContext(ctx, msg)

		if lc.CorrelationID != "ctx-corr" {
			t.Errorf("CorrelationID = %q, want %q", lc.CorrelationID, "ctx-corr")
		}
		if lc.TraceID != "ctx-trace" {
			t.Errorf("TraceID = %q, want %q", lc.TraceID, "ctx-trace")
		}
		if lc.SpanID != "ctx-span" {
			t.Errorf("SpanID = %q, want %q", lc.SpanID, "ctx-span")
		}

		// Context should have all IDs
		if GetCorrelationID(newCtx) != "ctx-corr" {
			t.Error("context missing correlation ID")
		}
	})

	t.Run("generate all", func(t *testing.T) {
		ctx := context.Background()
		msg := &types.Message{}

		newCtx, lc := ExtractOrGenerateLogContext(ctx, msg)

		if lc.CorrelationID == "" {
			t.Error("expected generated correlation ID")
		}
		if lc.TraceID == "" {
			t.Error("expected generated trace ID")
		}
		if lc.SpanID == "" {
			t.Error("expected generated span ID")
		}

		// Context should have all IDs
		if GetCorrelationID(newCtx) != lc.CorrelationID {
			t.Error("context correlation ID mismatch")
		}
		if GetTraceID(newCtx) != lc.TraceID {
			t.Error("context trace ID mismatch")
		}
		if GetSpanID(newCtx) != lc.SpanID {
			t.Error("context span ID mismatch")
		}
	})

	t.Run("from message metadata", func(t *testing.T) {
		ctx := context.Background()
		msg := &types.Message{
			Metadata: map[string]any{
				CorrelationIDMetadataKey: "msg-corr",
				TraceIDMetadataKey:       "msg-trace",
			},
		}

		_, lc := ExtractOrGenerateLogContext(ctx, msg)

		if lc.CorrelationID != "msg-corr" {
			t.Errorf("CorrelationID = %q, want %q", lc.CorrelationID, "msg-corr")
		}
		if lc.TraceID != "msg-trace" {
			t.Errorf("TraceID = %q, want %q", lc.TraceID, "msg-trace")
		}
		// SpanID should be generated since not in metadata
		if lc.SpanID == "" {
			t.Error("expected generated span ID")
		}
	})
}

// TestInjectLogContext validates setting all metadata fields.
func TestInjectLogContext(t *testing.T) {
	lc := LogContext{
		CorrelationID: "inject-corr",
		TraceID:       "inject-trace",
		SpanID:        "inject-span",
	}

	msg := &types.Message{}
	InjectLogContext(msg, lc)

	if msg.Metadata == nil {
		t.Fatal("expected metadata to be initialized")
	}
	if msg.Metadata[CorrelationIDMetadataKey] != "inject-corr" {
		t.Errorf("CorrelationID = %v, want %q", msg.Metadata[CorrelationIDMetadataKey], "inject-corr")
	}
	if msg.Metadata[TraceIDMetadataKey] != "inject-trace" {
		t.Errorf("TraceID = %v, want %q", msg.Metadata[TraceIDMetadataKey], "inject-trace")
	}
	if msg.Metadata[SpanIDMetadataKey] != "inject-span" {
		t.Errorf("SpanID = %v, want %q", msg.Metadata[SpanIDMetadataKey], "inject-span")
	}
}

// TestInjectLogContext_Partial validates partial injection.
func TestInjectLogContext_Partial(t *testing.T) {
	lc := LogContext{
		CorrelationID: "only-corr",
		// TraceID and SpanID empty
	}

	msg := &types.Message{}
	InjectLogContext(msg, lc)

	if msg.Metadata[CorrelationIDMetadataKey] != "only-corr" {
		t.Errorf("CorrelationID = %v, want %q", msg.Metadata[CorrelationIDMetadataKey], "only-corr")
	}
	// Empty IDs should not be injected
	if _, exists := msg.Metadata[TraceIDMetadataKey]; exists {
		t.Error("TraceID should not be injected when empty")
	}
	if _, exists := msg.Metadata[SpanIDMetadataKey]; exists {
		t.Error("SpanID should not be injected when empty")
	}
}

// TestTraceIDConstants validates trace ID constants.
func TestTraceIDConstants(t *testing.T) {
	if TraceIDMetadataKey != "_traceId" {
		t.Errorf("TraceIDMetadataKey = %q, want %q", TraceIDMetadataKey, "_traceId")
	}
	if TraceIDHeaderKey != "traceparent" {
		t.Errorf("TraceIDHeaderKey = %q, want %q", TraceIDHeaderKey, "traceparent")
	}
	if SpanIDMetadataKey != "_spanId" {
		t.Errorf("SpanIDMetadataKey = %q, want %q", SpanIDMetadataKey, "_spanId")
	}
	if RequestIDHeaderKey != "X-Request-ID" {
		t.Errorf("RequestIDHeaderKey = %q, want %q", RequestIDHeaderKey, "X-Request-ID")
	}
}

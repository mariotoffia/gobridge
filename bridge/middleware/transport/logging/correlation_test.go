package logging

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// Correlation ID Unit Tests
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

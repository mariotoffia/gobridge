package runtime

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/runtime/route"
)

// ═══════════════════════════════════════════════════════════════════
// MQTT Topic Validation Review Tests
//
// The MQTT-specific portion of these tests was moved to
// adapters/mqtt/transport/paho/topic_validator_test.go as part of
// AP-005 — MQTT topic validation is now a transport-supplied
// AddressValidator capability and the runtime no longer owns MQTT
// semantics. Only the transport-agnostic RenderAddress tests remain
// here.
// ═══════════════════════════════════════════════════════════════════

// TestRenderAddress_EmptyPlaceholderKey validates that an empty placeholder
// key like {} is rejected.
func TestRenderAddress_EmptyPlaceholderKey(t *testing.T) {
	_, err := route.RenderAddress("prefix/{}/suffix", map[string]any{"": "val"})
	if err == nil {
		t.Fatal("expected error for empty placeholder key")
	}
}

// TestRenderAddress_UnclosedBrace validates that an unclosed brace is
// treated as literal text and does not cause an error.
func TestRenderAddress_UnclosedBrace(t *testing.T) {
	result, err := route.RenderAddress("prefix/{unclosed", nil)
	if err != nil {
		t.Fatalf("unclosed brace should be treated as literal, got: %v", err)
	}
	if result != "prefix/{unclosed" {
		t.Fatalf("expected literal passthrough, got: %q", result)
	}
}

// TestRenderAddress_NoRecursiveExpansion validates that substituted values
// containing {key} patterns are NOT re-expanded (prevents injection).
func TestRenderAddress_NoRecursiveExpansion(t *testing.T) {
	headers := map[string]any{
		"device": "{secret}",
		"secret": "LEAKED",
	}
	result, err := route.RenderAddress("devices/{device}/data", headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result, "LEAKED") {
		t.Fatal("substituted values were re-expanded (injection vulnerability)")
	}
	if result != "devices/{secret}/data" {
		t.Fatalf("expected devices/{secret}/data, got: %q", result)
	}
}

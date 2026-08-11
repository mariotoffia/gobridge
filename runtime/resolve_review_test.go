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
// MQTT topic validation is now a transport-supplied
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

// TestRenderAddress_UnclosedBrace validates that an unclosed brace (an opening
// '{' with no closing '}') is rejected as a malformed template — symmetric with
// the missing-key error — rather than silently passed through as literal text.
func TestRenderAddress_UnclosedBrace(t *testing.T) {
	_, err := route.RenderAddress("prefix/{unclosed", nil)
	if err == nil {
		t.Fatal("unclosed brace should return an error, got nil")
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

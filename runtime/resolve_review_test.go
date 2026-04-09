package runtime

import (
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// MQTT Topic Validation Review Tests
//
// Validates edge cases identified by security audit (SEC-004, SEC-005):
//   - Maximum topic length enforcement (MQTT spec: 65,535 bytes)
//   - $SYS/ reserved topic prefix guard
//   - Additional edge cases for completeness
// ═══════════════════════════════════════════════════════════════════

// TestValidateMQTTTopic_MaxLength validates that topics exceeding the MQTT
// spec maximum of 65,535 bytes are rejected.
//
// ═══════════════════════════════════════════════════════════════════
// MQTT v5 spec §4.7: Topic names and filters are UTF-8 encoded
// strings that must not exceed 65,535 bytes.
//
// Input: topic of 65,536 bytes → REJECTED
// Input: topic of 65,535 bytes → ACCEPTED (if valid)
// ═══════════════════════════════════════════════════════════════════
func TestValidateMQTTTopic_MaxLength(t *testing.T) {
	longTopic := strings.Repeat("a", 65536)
	err := ValidateMQTTTopic(longTopic)
	if err == nil {
		t.Fatal("expected error for topic exceeding 65,535 bytes")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Fatalf("error should mention length, got: %v", err)
	}
}

// TestValidateMQTTTopic_ExactMaxLength validates that a topic of exactly
// 65,535 bytes is accepted (boundary condition).
func TestValidateMQTTTopic_ExactMaxLength(t *testing.T) {
	topic := strings.Repeat("a", 65535)
	err := ValidateMQTTTopic(topic)
	if err != nil {
		t.Fatalf("topic of exactly 65,535 bytes should be valid, got: %v", err)
	}
}

// TestValidateMQTTTopic_DollarPrefix validates that topics starting with $
// are rejected for publish operations (reserved by MQTT brokers).
//
// ═══════════════════════════════════════════════════════════════════
// MQTT v5 spec §4.7.2: Topics beginning with $ are reserved for
// the server. Clients must not publish to $-prefixed topics.
//
// Input: "$SYS/broker/load" → REJECTED
// Input: "$share/group/topic" → REJECTED
// Input: "devices/$status" → ACCEPTED ($ not at start)
// ═══════════════════════════════════════════════════════════════════
func TestValidateMQTTTopic_DollarPrefix(t *testing.T) {
	tests := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"$SYS prefix", "$SYS/broker/load", true},
		{"$share prefix", "$share/group/topic", true},
		{"$ alone", "$", true},
		{"dollar mid-topic", "devices/$status", false},
		{"normal topic", "devices/sensor/temp", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMQTTTopic(tt.topic)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for topic %q", tt.topic)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for topic %q: %v", tt.topic, err)
			}
		})
	}
}

// TestValidateMQTTTopic_ExistingValidation validates that existing checks
// still work correctly (regression guard).
func TestValidateMQTTTopic_ExistingValidation(t *testing.T) {
	tests := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"empty", "", true},
		{"wildcard +", "devices/+/temp", true},
		{"wildcard #", "devices/#", true},
		{"null byte", "devices/\x00/temp", true},
		{"empty segment", "devices//temp", true},
		{"valid", "devices/sensor/temp", false},
		{"single segment", "topic", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMQTTTopic(tt.topic)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for topic %q", tt.topic)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for topic %q: %v", tt.topic, err)
			}
		})
	}
}

// TestRenderAddress_EmptyPlaceholderKey validates that an empty placeholder
// key like {} is rejected.
func TestRenderAddress_EmptyPlaceholderKey(t *testing.T) {
	_, err := RenderAddress("prefix/{}/suffix", map[string]any{"": "val"})
	if err == nil {
		t.Fatal("expected error for empty placeholder key")
	}
}

// TestRenderAddress_UnclosedBrace validates that an unclosed brace is
// treated as literal text and does not cause an error.
func TestRenderAddress_UnclosedBrace(t *testing.T) {
	result, err := RenderAddress("prefix/{unclosed", nil)
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
	result, err := RenderAddress("devices/{device}/data", headers)
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

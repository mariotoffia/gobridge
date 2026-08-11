package messaging_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// ═══════════════════════════════════════════════════════════════════
// ParseTraceparent Edge Case Audit Tests
//
// Validates edge cases identified:
//   - Correct part count but wrong segment lengths
//   - Uppercase hex rejection (spec requires lowercase)
//   - All-zero trace ID and span ID rejection
// ═══════════════════════════════════════════════════════════════════

// TestParseTraceparent_PartialSegmentLengths validates that traceparent
// with 4 parts but incorrect segment lengths is rejected.
func TestParseTraceparent_PartialSegmentLengths(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"short trace ID", "00-abcdef0123456789abcdef012345678-0123456789abcdef-01"},
		{"long trace ID", "00-abcdef0123456789abcdef01234567899-0123456789abcdef-01"},
		{"short span ID", "00-abcdef0123456789abcdef0123456789-0123456789abcde-01"},
		{"long span ID", "00-abcdef0123456789abcdef0123456789-0123456789abcdef0-01"},
		{"short flags", "00-abcdef0123456789abcdef0123456789-0123456789abcdef-0"},
		{"long flags", "00-abcdef0123456789abcdef0123456789-0123456789abcdef-012"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := messaging.ParseTraceparent(tt.input)
			if ok {
				t.Fatalf("expected rejection for %q", tt.input)
			}
		})
	}
}

// TestParseTraceparent_UppercaseHex validates that uppercase hex
// characters are rejected (W3C spec requires lowercase).
func TestParseTraceparent_UppercaseHex(t *testing.T) {
	_, ok := messaging.ParseTraceparent("00-ABCDEF0123456789ABCDEF0123456789-0123456789ABCDEF-01")
	if ok {
		t.Fatal("uppercase hex should be rejected by W3C spec")
	}
}

// TestParseTraceparent_AllZeroTraceID validates rejection of all-zero
// trace ID (invalid per W3C spec).
func TestParseTraceparent_AllZeroTraceID(t *testing.T) {
	_, ok := messaging.ParseTraceparent("00-00000000000000000000000000000000-0123456789abcdef-01")
	if ok {
		t.Fatal("all-zero trace ID should be rejected")
	}
}

// TestParseTraceparent_AllZeroSpanID validates rejection of all-zero
// span ID (invalid per W3C spec).
func TestParseTraceparent_AllZeroSpanID(t *testing.T) {
	_, ok := messaging.ParseTraceparent("00-abcdef0123456789abcdef0123456789-0000000000000000-01")
	if ok {
		t.Fatal("all-zero span ID should be rejected")
	}
}

// TestParseTraceparent_ValidRoundtrip validates that a valid traceparent
// parses and re-formats correctly.
func TestParseTraceparent_ValidRoundtrip(t *testing.T) {
	input := "00-abcdef0123456789abcdef0123456789-0123456789abcdef-01"
	tc, ok := messaging.ParseTraceparent(input)
	if !ok {
		t.Fatalf("valid traceparent should parse: %q", input)
	}

	output := messaging.FormatTraceparent(tc)
	if output != input {
		t.Fatalf("roundtrip mismatch: %q != %q", output, input)
	}
}

// TestExtractTraceContext_WithState validates tracestate propagation.
func TestExtractTraceContext_WithState(t *testing.T) {
	headers := map[string]any{
		"traceparent": "00-abcdef0123456789abcdef0123456789-0123456789abcdef-01",
		"tracestate":  "vendor=value",
	}

	tc, ok := messaging.ExtractTraceContext(headers)
	if !ok {
		t.Fatal("should extract valid trace context")
	}
	if tc.State != "vendor=value" {
		t.Fatalf("expected tracestate %q, got %q", "vendor=value", tc.State)
	}
}

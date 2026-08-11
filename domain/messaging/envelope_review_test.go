package messaging_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// ═══════════════════════════════════════════════════════════════════
// Envelope Deep Copy Review Tests
//
// Validates deep copy completeness for header value types that were
// identified as missing by expert review (SEC-011).
// ═══════════════════════════════════════════════════════════════════

// TestEnvelope_Clone_DeepCopiesByteSliceHeaders validates that []byte header
// values are deep-copied so the clone and original do not share backing arrays.
//
// ═══════════════════════════════════════════════════════════════════
// Before fix: []byte header values share backing array between
//
//	original and clone, allowing cross-mutation.
//
// original.SetHeader("bin", []byte{0x01, 0x02})
// clone := original.Clone()
// clone.Headers()["bin"].([]byte)[0] = 0xFF
//
//	→ original.Headers()["bin"][0] == 0xFF  (WRONG - shared array)
//
// After fix: []byte values are copied to independent slices.
//
//	→ original.Headers()["bin"][0] == 0x01  (CORRECT - isolated)
//
// ═══════════════════════════════════════════════════════════════════
func TestEnvelope_Clone_DeepCopiesByteSliceHeaders(t *testing.T) {
	original := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "byte-test",
		Headers: map[string]any{
			"binary": []byte{0x01, 0x02, 0x03, 0x04},
		},
	})

	clone := original.Clone()

	cloneBytes := clone.Headers()["binary"].([]byte)
	cloneBytes[0] = 0xFF

	origBytes := original.Headers()["binary"].([]byte)
	if origBytes[0] == 0xFF {
		t.Fatal("[]byte header was not deep-copied: mutation leaked to original")
	}
	if origBytes[0] != 0x01 {
		t.Fatalf("expected original[0] = 0x01, got 0x%02x", origBytes[0])
	}
}

// TestEnvelope_Clone_ByteSliceInsideAnySlice validates that []byte values
// nested inside []any headers are also deep-copied.
func TestEnvelope_Clone_ByteSliceInsideAnySlice(t *testing.T) {
	original := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{
			"mixed": []any{[]byte{0xAA, 0xBB}, "text"},
		},
	})

	clone := original.Clone()

	inner := clone.Headers()["mixed"].([]any)
	innerBytes := inner[0].([]byte)
	innerBytes[0] = 0x00

	origInner := original.Headers()["mixed"].([]any)
	origBytes := origInner[0].([]byte)
	if origBytes[0] == 0x00 {
		t.Fatal("[]byte inside []any was not deep-copied")
	}
}

// TestEnvelope_Clone_ByteSliceInsideNestedMap validates that []byte values
// nested inside map[string]any headers are also deep-copied.
func TestEnvelope_Clone_ByteSliceInsideNestedMap(t *testing.T) {
	original := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{
			"nested": map[string]any{
				"data": []byte{0xDE, 0xAD},
			},
		},
	})

	clone := original.Clone()

	nested := clone.Headers()["nested"].(map[string]any)
	cloneData := nested["data"].([]byte)
	cloneData[0] = 0x00

	origNested := original.Headers()["nested"].(map[string]any)
	origData := origNested["data"].([]byte)
	if origData[0] == 0x00 {
		t.Fatal("[]byte inside nested map was not deep-copied")
	}
}

// TestEnvelope_Clone_EmptyByteSlice validates that empty []byte headers
// are cloned correctly (non-nil but empty).
func TestEnvelope_Clone_EmptyByteSlice(t *testing.T) {
	original := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{
			"empty": []byte{},
		},
	})

	clone := original.Clone()

	cloneBytes := clone.Headers()["empty"].([]byte)
	if cloneBytes == nil {
		t.Fatal("empty []byte should be non-nil after clone")
	}
	if len(cloneBytes) != 0 {
		t.Fatalf("expected empty []byte, got len=%d", len(cloneBytes))
	}
}

// TestEnvelope_Clone_NilByteSlice validates that nil []byte headers
// remain nil after cloning (falls through to default case).
func TestEnvelope_Clone_NilByteSlice(t *testing.T) {
	original := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{
			"nilbytes": ([]byte)(nil),
		},
	})

	clone := original.Clone()

	cloneVal := clone.Headers()["nilbytes"]
	if cloneVal != nil {
		if b, ok := cloneVal.([]byte); ok && b != nil {
			t.Fatal("nil []byte should remain nil after clone")
		}
	}
}

// TestEnvelope_Clone_LargePayload validates copy-on-write transformation of a
// large shared immutable payload without truncation or corruption.
func TestEnvelope_Clone_LargePayload(t *testing.T) {
	payload := make([]byte, 1<<20) // 1 MiB
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	original := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "large",
		Payload: payload,
	})

	clone := original.Clone()

	if len(clone.Payload()) != len(original.Payload()) {
		t.Fatalf("payload length mismatch: got %d, want %d", len(clone.Payload()), len(original.Payload()))
	}

	cloneP := clone.Payload()
	cloneP[0] = 0xFF
	clone.SetPayload(cloneP)
	if original.Payload()[0] == 0xFF {
		t.Fatal("large payload transformation aliased original backing")
	}
}

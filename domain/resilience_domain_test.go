package domain

// ═══════════════════════════════════════════════
// Domain Resilience Tests
//
// Additional tests for domain types identified
// during expert review.
//
// Summary:
// ┌──────┬────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                │ Status   │
// ├──────┼────────────────────────────────────────────┼──────────┤
// │ T001 │ Clone produces independent deep copy       │ PASS     │
// │ T002 │ Clone nil payload stays nil                │ PASS     │
// │ T003 │ MergeHeaders protects reserved case-insens.│ PASS     │
// │ T004 │ StripReservedHeaders is idempotent         │ PASS     │
// │ T005 │ BridgeError.Is matches by code             │ PASS     │
// │ T006 │ IsRecoverableError treats unknown as true  │ PASS     │
// │ T007 │ BridgeError.With clones context map        │ PASS     │
// │ T008 │ OutboxPartitionKey deterministic           │ PASS     │
// └──────┴────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════

import (
	"errors"
	"testing"
	"time"
)

// TestClone_DeepCopy_Independence validates that modifying a clone
// does not affect the original (QA-M10 property).
func TestClone_DeepCopy_Independence(t *testing.T) {
	original := &Envelope{
		ID:        "orig",
		Subject:   "test",
		Payload:   []byte("hello world"),
		Headers:   map[string]any{"key": "val"},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}

	clone := original.Clone()

	clone.ID = "cloned"
	clone.Subject = "modified"
	clone.Payload[0] = 'X'
	clone.Headers["key"] = "modified"
	clone.Headers["new"] = "added"

	if original.ID != "orig" {
		t.Fatalf("original ID was modified: %s", original.ID)
	}
	if original.Subject != "test" {
		t.Fatalf("original Subject was modified: %s", original.Subject)
	}
	if original.Payload[0] != 'h' {
		t.Fatalf("original Payload was modified: %s", string(original.Payload))
	}
	if original.Headers["key"] != "val" {
		t.Fatalf("original Header was modified: %v", original.Headers["key"])
	}
	if _, exists := original.Headers["new"]; exists {
		t.Fatal("new header leaked to original")
	}
}

// TestClone_NilPayload validates that cloning with nil payload doesn't
// allocate an empty slice.
func TestClone_NilPayload(t *testing.T) {
	original := &Envelope{
		ID:      "no-payload",
		Subject: "test",
		Headers: map[string]any{"k": "v"},
	}

	clone := original.Clone()
	if clone.Payload != nil {
		t.Fatal("clone Payload should be nil when original is nil")
	}
	if clone.Headers == nil {
		t.Fatal("clone Headers should not be nil when original has headers")
	}
}

// TestClone_NilHeaders validates clone with nil headers.
func TestClone_NilHeaders(t *testing.T) {
	original := &Envelope{
		ID:      "no-headers",
		Subject: "test",
		Payload: []byte("data"),
	}

	clone := original.Clone()
	if clone.Headers != nil {
		t.Fatal("clone Headers should be nil when original is nil")
	}
}

// TestMergeHeaders_ProtectsReserved_CaseInsensitive validates that
// reserved header protection works with mixed-case keys.
func TestMergeHeaders_ProtectsReserved_CaseInsensitive(t *testing.T) {
	base := map[string]any{
		HeaderCorrelationID: "original-corr",
		"custom":            "base-value",
	}
	overlay := map[string]any{
		"X-Bridge.Correlation-Id": "injected-corr",
		"custom":                  "overlay-value",
	}

	result := MergeHeaders(base, overlay, true)

	corrID, ok := result[HeaderCorrelationID]
	if !ok {
		t.Fatal("expected correlation ID in result")
	}
	if corrID != "original-corr" {
		t.Fatalf("reserved header should be protected, got %v", corrID)
	}
	if result["custom"] != "overlay-value" {
		t.Fatal("non-reserved headers should be overridden")
	}
}

// TestStripReservedHeaders_Idempotent validates that stripping is
// idempotent: strip(strip(h)) == strip(h).
func TestStripReservedHeaders_Idempotent(t *testing.T) {
	headers := map[string]any{
		HeaderCorrelationID: "corr-1",
		HeaderRouteID:       "route-1",
		"custom-header":     "value",
		"another":           42,
	}

	first := StripReservedHeaders(headers)
	second := StripReservedHeaders(first)

	if len(first) != len(second) {
		t.Fatalf("idempotent violation: first=%d, second=%d entries", len(first), len(second))
	}
	for k, v := range first {
		if second[k] != v {
			t.Fatalf("idempotent violation at key %q: %v vs %v", k, v, second[k])
		}
	}
	if _, exists := first[HeaderCorrelationID]; exists {
		t.Fatal("reserved header should be stripped")
	}
}

// TestBridgeError_Is_MatchesByCode validates errors.Is matching by
// error code, not by identity.
func TestBridgeError_Is_MatchesByCode(t *testing.T) {
	wrapped := ErrConnectionLost.Wrap(errors.New("tcp: connection reset"))

	if !errors.Is(wrapped, ErrConnectionLost) {
		t.Fatal("wrapped error should match sentinel by code")
	}

	different := ErrTimeout.Wrap(errors.New("context deadline"))
	if errors.Is(wrapped, different) {
		t.Fatal("different error codes should not match")
	}
}

// TestIsRecoverableError_UnknownAsTrue validates that non-BridgeError
// types are treated as recoverable (safe default for retry).
func TestIsRecoverableError_UnknownAsTrue(t *testing.T) {
	plain := errors.New("generic error")
	if !IsRecoverableError(plain) {
		t.Fatal("non-BridgeError should be treated as recoverable")
	}
}

// TestIsRecoverableError_Nil validates nil error handling.
func TestIsRecoverableError_Nil(t *testing.T) {
	if IsRecoverableError(nil) {
		t.Fatal("nil error should not be recoverable")
	}
}

// TestBridgeError_With_ClonesContext validates that With() creates
// an independent context map.
func TestBridgeError_With_ClonesContext(t *testing.T) {
	base := ErrConnectionLost.With("host", "broker-1")
	derived := base.With("port", 1883)

	if _, ok := base.Context["port"]; ok {
		t.Fatal("With() should not modify the source error context")
	}
	if derived.Context["host"] != "broker-1" {
		t.Fatal("derived should inherit base context")
	}
	if derived.Context["port"] != 1883 {
		t.Fatal("derived should have new context key")
	}
}

// TestOutboxPartitionKey_Deterministic validates that partition keys
// are deterministic and non-empty for valid inputs.
func TestOutboxPartitionKey_Deterministic(t *testing.T) {
	tests := []struct {
		sessionID string
		bindingID string
		expected  string
	}{
		{"sess-1", "", "SESSION#sess-1"},
		{"", "bind-1", "BINDING#bind-1"},
		{"sess-1", "bind-1", "SESSION#sess-1"},
	}

	for _, tc := range tests {
		got := OutboxPartitionKey(tc.sessionID, tc.bindingID)
		if got != tc.expected {
			t.Fatalf("OutboxPartitionKey(%q, %q) = %q, want %q",
				tc.sessionID, tc.bindingID, got, tc.expected)
		}
		got2 := OutboxPartitionKey(tc.sessionID, tc.bindingID)
		if got != got2 {
			t.Fatal("OutboxPartitionKey should be deterministic")
		}
	}
}

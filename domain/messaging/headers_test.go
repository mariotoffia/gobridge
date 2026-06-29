package messaging_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestIsReservedHeader validates IsReservedHeader for bridge-reserved keys, custom prefixes, and ordinary headers.
func TestIsReservedHeader(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{messaging.HeaderCorrelationID, true},
		{messaging.HeaderRouteID, true},
		{"x-bridge.custom", true},
		{"traceparent", false},
		{"my-header", false},
		{"", false},
		{messaging.HeaderTenantID, true},
		{messaging.HeaderRouteOverride, true},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := messaging.IsReservedHeader(tt.key); got != tt.want {
				t.Fatalf("IsReservedHeader(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestStripReservedHeaders verifies StripReservedHeaders removes reserved keys, keeps others, copies into a new map, and leaves the input unchanged.
func TestStripReservedHeaders(t *testing.T) {
	headers := map[string]any{
		messaging.HeaderCorrelationID: "abc",
		messaging.HeaderSourceID:      "src-1",
		"traceparent":                 "00-trace",
		"custom":                      "value",
	}
	stripped := messaging.StripReservedHeaders(headers)

	if _, ok := stripped[messaging.HeaderCorrelationID]; ok {
		t.Fatal("reserved header should be stripped")
	}
	if _, ok := stripped["traceparent"]; !ok {
		t.Fatal("non-reserved header should be kept")
	}
	if _, ok := stripped["custom"]; !ok {
		t.Fatal("custom header should be kept")
	}
	// Original map unmodified.
	if _, ok := headers[messaging.HeaderCorrelationID]; !ok {
		t.Fatal("original map should not be modified")
	}
}

// TestStripReservedHeaders_Nil verifies StripReservedHeaders returns nil when the input map is nil.
func TestStripReservedHeaders_Nil(t *testing.T) {
	if got := messaging.StripReservedHeaders(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// TestMergeHeaders verifies MergeHeaders overlays keys and optionally preserves reserved keys already present in the base map.
func TestMergeHeaders(t *testing.T) {
	base := map[string]any{
		messaging.HeaderRouteID: "route-1",
		"custom":                "base",
	}
	overlay := map[string]any{
		messaging.HeaderRouteID: "route-override",
		"custom":                "overlay",
		"extra":                 "new",
	}

	// Without protection: overlay wins on all keys.
	merged := messaging.MergeHeaders(base, overlay, false)
	if merged[messaging.HeaderRouteID] != "route-override" {
		t.Fatal("overlay should override without protection")
	}
	if merged["custom"] != "overlay" {
		t.Fatal("overlay should override custom key")
	}
	if merged["extra"] != "new" {
		t.Fatal("new key from overlay should appear")
	}

	// With protection: reserved keys in base are protected.
	merged = messaging.MergeHeaders(base, overlay, true)
	if merged[messaging.HeaderRouteID] != "route-1" {
		t.Fatal("reserved key in base should be protected")
	}
	if merged["custom"] != "overlay" {
		t.Fatal("non-reserved key should still be overridden")
	}
}

// TestMergeHeaders_NilInputs verifies MergeHeaders handles nil base and overlay combinations.
func TestMergeHeaders_NilInputs(t *testing.T) {
	if got := messaging.MergeHeaders(nil, nil, false); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	result := messaging.MergeHeaders(nil, map[string]any{"a": 1}, false)
	if result["a"] != 1 {
		t.Fatal("should merge overlay into empty base")
	}
	result = messaging.MergeHeaders(map[string]any{"a": 1}, nil, false)
	if result["a"] != 1 {
		t.Fatal("should preserve base when overlay is nil")
	}
}

// TestMergeHeaders_ProtectReserved_CaseInsensitive verifies that protection
// works even when the base and overlay use different casing of the same
// reserved header key.
func TestMergeHeaders_ProtectReserved_CaseInsensitive(t *testing.T) {
	base := map[string]any{
		"x-bridge.route-id": "original",
		"custom":            "base",
	}
	overlay := map[string]any{
		"X-Bridge.Route-Id": "injected",
		"custom":            "overlay",
	}

	merged := messaging.MergeHeaders(base, overlay, true)

	if merged["x-bridge.route-id"] != "original" {
		t.Fatal("original reserved key should be preserved")
	}
	if _, ok := merged["X-Bridge.Route-Id"]; ok {
		t.Fatal("mixed-case duplicate reserved key should not be added")
	}
	if merged["custom"] != "overlay" {
		t.Fatal("non-reserved key should still be overridden")
	}
}

// TestGetHeaderString verifies GetHeaderString returns string values, rejects wrong types and missing keys, and handles nil maps.
func TestGetHeaderString(t *testing.T) {
	headers := map[string]any{
		"str":    "value",
		"notstr": 42,
	}
	if v, ok := messaging.GetHeaderString(headers, "str"); !ok || v != "value" {
		t.Fatal("expected string value")
	}
	if _, ok := messaging.GetHeaderString(headers, "notstr"); ok {
		t.Fatal("expected false for non-string value")
	}
	if _, ok := messaging.GetHeaderString(headers, "missing"); ok {
		t.Fatal("expected false for missing key")
	}
	if _, ok := messaging.GetHeaderString(nil, "key"); ok {
		t.Fatal("expected false for nil headers")
	}
}

// TestSetHeader verifies SetHeader initializes a new map from nil and appends keys on an existing map.
func TestSetHeader(t *testing.T) {
	h := messaging.SetHeader(nil, "key", "value")
	if h["key"] != "value" {
		t.Fatal("expected key to be set")
	}

	h = messaging.SetHeader(h, "key2", 42)
	if h["key2"] != 42 || h["key"] != "value" {
		t.Fatal("expected both keys to be present")
	}
}

// TestIsReservedHeader_CaseInsensitive validates that mixed-case attempts
// to bypass the reserved prefix are caught.
func TestIsReservedHeader_CaseInsensitive(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"X-Bridge.route-id", true},
		{"X-BRIDGE.ROUTE-ID", true},
		{"x-BRIDGE.tenant-id", true},
		{"X-bridge.Custom", true},
		{"x-Bridge.forwarded-from", true},
		{"x-bridg", false},
		{"x-bridge", false},
		{"x-bridge.", true},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := messaging.IsReservedHeader(tc.key); got != tc.want {
				t.Fatalf("IsReservedHeader(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// TestStripReservedHeaders_CaseInsensitive validates that StripReservedHeaders
// removes mixed-case reserved headers.
func TestStripReservedHeaders_CaseInsensitive(t *testing.T) {
	headers := map[string]any{
		"X-Bridge.route-id":  "injected",
		"X-BRIDGE.SOURCE-ID": "injected",
		"safe-header":        "keep",
	}
	stripped := messaging.StripReservedHeaders(headers)

	if _, ok := stripped["X-Bridge.route-id"]; ok {
		t.Error("mixed-case reserved header should be stripped")
	}
	if _, ok := stripped["X-BRIDGE.SOURCE-ID"]; ok {
		t.Error("uppercase reserved header should be stripped")
	}
	if stripped["safe-header"] != "keep" {
		t.Error("safe header should be preserved")
	}
}

// TestStripReservedHeaders_EmptyMap validates StripReservedHeaders returns
// an empty map (not nil) for a non-nil empty input.
func TestStripReservedHeaders_EmptyMap(t *testing.T) {
	result := messaging.StripReservedHeaders(map[string]any{})
	if result == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(result) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(result))
	}
}

// TestMergeHeaders_ProtectReserved_NewReservedKey validates that
// protectReserved allows new reserved keys from overlay when base
// does not already have them.
func TestMergeHeaders_ProtectReserved_NewReservedKey(t *testing.T) {
	base := map[string]any{"custom": "base"}
	overlay := map[string]any{
		messaging.HeaderCorrelationID: "new-corr",
		"custom":                      "overlay",
	}

	merged := messaging.MergeHeaders(base, overlay, true)
	if merged[messaging.HeaderCorrelationID] != "new-corr" {
		t.Fatal("new reserved key from overlay should be added when not in base")
	}
	if merged["custom"] != "overlay" {
		t.Fatal("non-reserved key should still be overridden")
	}
}

// TestSetHeader_ReturnValueRequired validates that SetHeader's return value
// must be captured when input is nil.
func TestSetHeader_ReturnValueRequired(t *testing.T) {
	var h map[string]any
	h = messaging.SetHeader(h, "a", 1)
	h = messaging.SetHeader(h, "b", 2)
	if len(h) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(h))
	}
	if h["a"] != 1 || h["b"] != 2 {
		t.Fatal("expected both values to be present")
	}
}

// TestHeaderClassification asserts every reserved header constant is in
// exactly one classification bucket (INTERNAL-ONLY xor BRIDGE-TO-BRIDGE
// PROPAGATED) — exhaustive and disjoint.
func TestHeaderClassification(t *testing.T) {
	internalOnly := []string{
		messaging.HeaderRouteID,
		messaging.HeaderRouteOverride,
		messaging.HeaderSourceID,
		messaging.HeaderContentType,
	}
	propagated := []string{
		messaging.HeaderCorrelationID,
		messaging.HeaderCausationID,
		messaging.HeaderIdempotencyKey,
		messaging.HeaderDeduplicationID,
		messaging.HeaderOrderingKey,
		messaging.HeaderTenantID,
		messaging.HeaderForwardedFrom,
		messaging.HeaderForwardedHop,
		messaging.HeaderTraceParent,
		messaging.HeaderTraceState,
	}
	for _, k := range internalOnly {
		t.Run("internal/"+k, func(t *testing.T) {
			if !messaging.IsInternalOnlyHeader(k) {
				t.Fatalf("IsInternalOnlyHeader(%q) = false, want true", k)
			}
			if messaging.IsBridgeToBridgeHeader(k) {
				t.Fatalf("IsBridgeToBridgeHeader(%q) = true, want false (disjoint)", k)
			}
		})
	}
	for _, k := range propagated {
		t.Run("propagated/"+k, func(t *testing.T) {
			if !messaging.IsBridgeToBridgeHeader(k) {
				t.Fatalf("IsBridgeToBridgeHeader(%q) = false, want true", k)
			}
			if messaging.IsInternalOnlyHeader(k) {
				t.Fatalf("IsInternalOnlyHeader(%q) = true, want false (disjoint)", k)
			}
		})
	}
}

// TestHeaderClassification_NonReserved confirms an application key is
// neither INTERNAL-ONLY nor BRIDGE-TO-BRIDGE PROPAGATED.
func TestHeaderClassification_NonReserved(t *testing.T) {
	for _, k := range []string{"my-header", "tenant", "gobridge.subject", ""} {
		if messaging.IsInternalOnlyHeader(k) {
			t.Fatalf("IsInternalOnlyHeader(%q) = true, want false", k)
		}
		if messaging.IsBridgeToBridgeHeader(k) {
			t.Fatalf("IsBridgeToBridgeHeader(%q) = true, want false", k)
		}
	}
}

// TestHeaderClassification_CaseInsensitive confirms the predicates fold
// case, consistent with IsReservedHeader.
func TestHeaderClassification_CaseInsensitive(t *testing.T) {
	if !messaging.IsInternalOnlyHeader("X-Bridge.Route-ID") {
		t.Fatal("IsInternalOnlyHeader must be case-insensitive")
	}
	if !messaging.IsBridgeToBridgeHeader("X-BRIDGE.IDEMPOTENCY-KEY") {
		t.Fatal("IsBridgeToBridgeHeader must be case-insensitive")
	}
	if !messaging.IsBridgeToBridgeHeader("TraceParent") {
		t.Fatal("IsBridgeToBridgeHeader must fold traceparent")
	}
}

// TestStripInternalOnlyHeaders removes INTERNAL-ONLY reserved headers
// while preserving BRIDGE-TO-BRIDGE PROPAGATED and application headers,
// and does not mutate its input.
func TestStripInternalOnlyHeaders(t *testing.T) {
	in := map[string]any{
		messaging.HeaderRouteID:        "rt",       // internal-only -> dropped
		messaging.HeaderSourceID:       "src",      // internal-only -> dropped
		messaging.HeaderContentType:    "app/json", // internal-only -> dropped
		messaging.HeaderRouteOverride:  "ovr",      // internal-only -> dropped
		messaging.HeaderCorrelationID:  "corr",     // propagated -> kept
		messaging.HeaderIdempotencyKey: "idem",     // propagated -> kept
		messaging.HeaderTraceParent:    "tp",       // propagated -> kept
		"app-key":                      "kept",     // application -> kept
	}
	out := messaging.StripInternalOnlyHeaders(in)
	for _, k := range []string{
		messaging.HeaderRouteID, messaging.HeaderSourceID,
		messaging.HeaderContentType, messaging.HeaderRouteOverride,
	} {
		if _, ok := out[k]; ok {
			t.Fatalf("internal-only header %q must be stripped", k)
		}
	}
	for _, k := range []string{
		messaging.HeaderCorrelationID, messaging.HeaderIdempotencyKey,
		messaging.HeaderTraceParent, "app-key",
	} {
		if _, ok := out[k]; !ok {
			t.Fatalf("header %q must be preserved", k)
		}
	}
	if _, ok := in[messaging.HeaderRouteID]; !ok {
		t.Fatal("StripInternalOnlyHeaders must not mutate its input")
	}
}

// TestStripInternalOnlyHeaders_Nil returns nil for nil input.
func TestStripInternalOnlyHeaders_Nil(t *testing.T) {
	if messaging.StripInternalOnlyHeaders(nil) != nil {
		t.Fatal("StripInternalOnlyHeaders(nil) must be nil")
	}
}

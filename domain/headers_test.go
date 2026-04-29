package domain_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

// TestIsReservedHeader validates IsReservedHeader for bridge-reserved keys, custom prefixes, and ordinary headers.
func TestIsReservedHeader(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{domain.HeaderCorrelationID, true},
		{domain.HeaderRouteID, true},
		{"x-bridge.custom", true},
		{"traceparent", false},
		{"my-header", false},
		{"", false},
		{domain.HeaderTenantID, true},
		{domain.HeaderRouteOverride, true},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := domain.IsReservedHeader(tt.key); got != tt.want {
				t.Fatalf("IsReservedHeader(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestStripReservedHeaders verifies StripReservedHeaders removes reserved keys, keeps others, copies into a new map, and leaves the input unchanged.
func TestStripReservedHeaders(t *testing.T) {
	headers := map[string]any{
		domain.HeaderCorrelationID: "abc",
		domain.HeaderSourceID:      "src-1",
		"traceparent":              "00-trace",
		"custom":                   "value",
	}
	stripped := domain.StripReservedHeaders(headers)

	if _, ok := stripped[domain.HeaderCorrelationID]; ok {
		t.Fatal("reserved header should be stripped")
	}
	if _, ok := stripped["traceparent"]; !ok {
		t.Fatal("non-reserved header should be kept")
	}
	if _, ok := stripped["custom"]; !ok {
		t.Fatal("custom header should be kept")
	}
	// Original map unmodified.
	if _, ok := headers[domain.HeaderCorrelationID]; !ok {
		t.Fatal("original map should not be modified")
	}
}

// TestStripReservedHeaders_Nil verifies StripReservedHeaders returns nil when the input map is nil.
func TestStripReservedHeaders_Nil(t *testing.T) {
	if got := domain.StripReservedHeaders(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// TestMergeHeaders verifies MergeHeaders overlays keys and optionally preserves reserved keys already present in the base map.
func TestMergeHeaders(t *testing.T) {
	base := map[string]any{
		domain.HeaderRouteID: "route-1",
		"custom":             "base",
	}
	overlay := map[string]any{
		domain.HeaderRouteID: "route-override",
		"custom":             "overlay",
		"extra":              "new",
	}

	// Without protection: overlay wins on all keys.
	merged := domain.MergeHeaders(base, overlay, false)
	if merged[domain.HeaderRouteID] != "route-override" {
		t.Fatal("overlay should override without protection")
	}
	if merged["custom"] != "overlay" {
		t.Fatal("overlay should override custom key")
	}
	if merged["extra"] != "new" {
		t.Fatal("new key from overlay should appear")
	}

	// With protection: reserved keys in base are protected.
	merged = domain.MergeHeaders(base, overlay, true)
	if merged[domain.HeaderRouteID] != "route-1" {
		t.Fatal("reserved key in base should be protected")
	}
	if merged["custom"] != "overlay" {
		t.Fatal("non-reserved key should still be overridden")
	}
}

// TestMergeHeaders_NilInputs verifies MergeHeaders handles nil base and overlay combinations.
func TestMergeHeaders_NilInputs(t *testing.T) {
	if got := domain.MergeHeaders(nil, nil, false); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	result := domain.MergeHeaders(nil, map[string]any{"a": 1}, false)
	if result["a"] != 1 {
		t.Fatal("should merge overlay into empty base")
	}
	result = domain.MergeHeaders(map[string]any{"a": 1}, nil, false)
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

	merged := domain.MergeHeaders(base, overlay, true)

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
	if v, ok := domain.GetHeaderString(headers, "str"); !ok || v != "value" {
		t.Fatal("expected string value")
	}
	if _, ok := domain.GetHeaderString(headers, "notstr"); ok {
		t.Fatal("expected false for non-string value")
	}
	if _, ok := domain.GetHeaderString(headers, "missing"); ok {
		t.Fatal("expected false for missing key")
	}
	if _, ok := domain.GetHeaderString(nil, "key"); ok {
		t.Fatal("expected false for nil headers")
	}
}

// TestSetHeader verifies SetHeader initializes a new map from nil and appends keys on an existing map.
func TestSetHeader(t *testing.T) {
	h := domain.SetHeader(nil, "key", "value")
	if h["key"] != "value" {
		t.Fatal("expected key to be set")
	}

	h = domain.SetHeader(h, "key2", 42)
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
			if got := domain.IsReservedHeader(tc.key); got != tc.want {
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
	stripped := domain.StripReservedHeaders(headers)

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
	result := domain.StripReservedHeaders(map[string]any{})
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
		domain.HeaderCorrelationID: "new-corr",
		"custom":                   "overlay",
	}

	merged := domain.MergeHeaders(base, overlay, true)
	if merged[domain.HeaderCorrelationID] != "new-corr" {
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
	h = domain.SetHeader(h, "a", 1)
	h = domain.SetHeader(h, "b", 2)
	if len(h) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(h))
	}
	if h["a"] != 1 || h["b"] != 2 {
		t.Fatal("expected both values to be present")
	}
}

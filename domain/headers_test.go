package domain_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

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

func TestStripReservedHeaders_Nil(t *testing.T) {
	if got := domain.StripReservedHeaders(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

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

func TestMergeHeaders_NilInputs(t *testing.T) {
	if got := domain.MergeHeaders(nil, nil, false); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	result := domain.MergeHeaders(nil, map[string]any{"a": 1}, false)
	if result["a"] != 1 {
		t.Fatal("should merge overlay into empty base")
	}
}

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

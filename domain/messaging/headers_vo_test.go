package messaging_test

import (
	"sort"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestNewHeaders_EmptyNonNil verifies NewHeaders returns a usable
// non-nil Headers ready for Set.
func TestNewHeaders_EmptyNonNil(t *testing.T) {
	h := messaging.NewHeaders()
	if h == nil {
		t.Fatal("NewHeaders should not return nil")
	}
	if !h.IsEmpty() {
		t.Fatalf("expected empty, got Len=%d", h.Len())
	}
	h.Set("k", "v")
	if got, ok := h.Get("k"); !ok || got != "v" {
		t.Fatalf("Set/Get round-trip failed: %v ok=%v", got, ok)
	}
}

// TestNewHeadersFromMap_StripsReserved verifies the constructor strips
// reserved-prefix keys, mirroring the historical StripReservedHeaders
// contract used by Envelope ingress.
func TestNewHeadersFromMap_StripsReserved(t *testing.T) {
	in := map[string]any{
		messaging.HeaderCorrelationID: "spoofed",
		"X-Bridge.Route-Id":           "spoofed-mixed-case",
		"safe":                        "kept",
	}
	h := messaging.NewHeadersFromMap(in)
	if h.Has(messaging.HeaderCorrelationID) {
		t.Error("reserved key should be stripped at construction")
	}
	if h.Has("X-Bridge.Route-Id") {
		t.Error("mixed-case reserved key should be stripped at construction")
	}
	if v, ok := h.GetString("safe"); !ok || v != "kept" {
		t.Errorf("safe key dropped: %v ok=%v", v, ok)
	}
	if _, ok := in[messaging.HeaderCorrelationID]; !ok {
		t.Error("input map must not be mutated by constructor")
	}
}

// TestNewHeadersFromMap_NilReturnsNil mirrors StripReservedHeaders(nil).
func TestNewHeadersFromMap_NilReturnsNil(t *testing.T) {
	if got := messaging.NewHeadersFromMap(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// TestHeaders_NilSafeReaders verifies Get / GetString / Has / Len /
// IsEmpty / Range / Snapshot / AsMap / Delete / convenience accessors
// all behave on a nil receiver.
func TestHeaders_NilSafeReaders(t *testing.T) {
	var h messaging.Headers
	if v, ok := h.Get("k"); ok || v != nil {
		t.Errorf("Get on nil: got %v ok=%v", v, ok)
	}
	if v, ok := h.GetString("k"); ok || v != "" {
		t.Errorf("GetString on nil: got %q ok=%v", v, ok)
	}
	if h.Has("k") {
		t.Error("Has on nil should be false")
	}
	if h.Len() != 0 {
		t.Errorf("Len on nil = %d, want 0", h.Len())
	}
	if !h.IsEmpty() {
		t.Error("IsEmpty on nil should be true")
	}
	called := false
	h.Range(func(string, any) bool { called = true; return true })
	if called {
		t.Error("Range on nil should not invoke callback")
	}
	if got := h.Snapshot(); got != nil {
		t.Errorf("Snapshot on nil = %v, want nil", got)
	}
	if got := h.AsMap(); got != nil {
		t.Errorf("AsMap on nil = %v, want nil", got)
	}
	h.Delete("k") // must not panic
	if _, ok := h.CorrelationID(); ok {
		t.Error("CorrelationID on nil should be false")
	}
	if _, ok := h.RouteID(); ok {
		t.Error("RouteID on nil should be false")
	}
	if _, ok := h.RouteOverride(); ok {
		t.Error("RouteOverride on nil should be false")
	}
}

// TestHeaders_SetOnNilPanics documents the contract: callers holding a
// nil Headers must not call Set; Envelope.SetHeader is the lazily-
// allocating bridge for that case.
func TestHeaders_SetOnNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Set on nil receiver should panic")
		}
	}()
	var h messaging.Headers
	h.Set("k", "v")
}

// TestHeaders_GetStringTypeMismatch verifies non-string values yield
// ok=false.
func TestHeaders_GetStringTypeMismatch(t *testing.T) {
	h := messaging.Headers{"n": 42}
	if v, ok := h.GetString("n"); ok || v != "" {
		t.Errorf("GetString on int value: got %q ok=%v", v, ok)
	}
}

// TestHeaders_RangeIteratesAllAndStopsEarly verifies Range visits every
// key by default and honours the early-stop boolean.
func TestHeaders_RangeIteratesAllAndStopsEarly(t *testing.T) {
	h := messaging.Headers{"a": 1, "b": 2, "c": 3}

	var keys []string
	h.Range(func(k string, _ any) bool { keys = append(keys, k); return true })
	sort.Strings(keys)
	if len(keys) != 3 || keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Fatalf("Range did not visit all keys: %v", keys)
	}

	count := 0
	h.Range(func(string, any) bool { count++; return false })
	if count != 1 {
		t.Errorf("early-stop Range visited %d entries, want 1", count)
	}
}

// TestHeaders_Merge_ProtectReservedCaseInsensitive parity-checks the
// method form against the package-level MergeHeaders helper.
func TestHeaders_Merge_ProtectReservedCaseInsensitive(t *testing.T) {
	base := messaging.Headers{
		"x-bridge.route-id": "original",
		"custom":            "base",
	}
	overlay := messaging.Headers{
		"X-Bridge.Route-Id": "injected",
		"custom":            "overlay",
		"extra":             "added",
	}
	merged := base.Merge(overlay, true)
	if v, _ := merged.GetString("x-bridge.route-id"); v != "original" {
		t.Errorf("reserved key should be protected: got %q", v)
	}
	if merged.Has("X-Bridge.Route-Id") {
		t.Error("mixed-case duplicate should not slip through")
	}
	if v, _ := merged.GetString("custom"); v != "overlay" {
		t.Errorf("non-reserved should be overridden: got %q", v)
	}
	if v, _ := merged.GetString("extra"); v != "added" {
		t.Errorf("new overlay key should appear: got %q", v)
	}
}

// TestHeaders_MergeBothEmpty returns nil to match MergeHeaders.
func TestHeaders_MergeBothEmpty(t *testing.T) {
	var a, b messaging.Headers
	if got := a.Merge(b, true); got != nil {
		t.Fatalf("Merge of two empties should be nil, got %v", got)
	}
}

// TestHeaders_SnapshotIsDeepCopy ensures mutating the snapshot does not
// affect the source headers, even for nested map / slice values.
func TestHeaders_SnapshotIsDeepCopy(t *testing.T) {
	h := messaging.Headers{
		"nested": map[string]any{"k": "v"},
		"list":   []any{1, 2, 3},
	}
	snap := h.Snapshot()
	snap["nested"].(map[string]any)["k"] = "tampered"
	snap["list"].([]any)[0] = 999

	if v := h["nested"].(map[string]any)["k"]; v != "v" {
		t.Errorf("source nested map mutated through snapshot: %v", v)
	}
	if v := h["list"].([]any)[0]; v != 1 {
		t.Errorf("source list mutated through snapshot: %v", v)
	}
}

// TestHeaders_AsMapAliasesUnderlying confirms AsMap is a zero-copy
// view, not a defensive copy. This documents the transport-escape-
// hatch contract.
func TestHeaders_AsMapAliasesUnderlying(t *testing.T) {
	h := messaging.NewHeaders()
	h.Set("k", "v")
	m := h.AsMap()
	m["k"] = "mutated"
	if v, _ := h.GetString("k"); v != "mutated" {
		t.Errorf("AsMap should return live alias, got %q", v)
	}
}

// TestHeaders_ConvenienceAccessors verifies the well-known accessor
// shortcuts read the canonical header keys.
func TestHeaders_ConvenienceAccessors(t *testing.T) {
	h := messaging.Headers{
		messaging.HeaderCorrelationID: "corr-1",
		messaging.HeaderRouteID:       "route-1",
		messaging.HeaderRouteOverride: "queue-A",
	}
	if v, ok := h.CorrelationID(); !ok || v != "corr-1" {
		t.Errorf("CorrelationID = %q ok=%v", v, ok)
	}
	if v, ok := h.RouteID(); !ok || v != "route-1" {
		t.Errorf("RouteID = %q ok=%v", v, ok)
	}
	if v, ok := h.RouteOverride(); !ok || v != "queue-A" {
		t.Errorf("RouteOverride = %q ok=%v", v, ok)
	}
}

// TestHeaders_AssignableToMapStringAny is a compile-time-style check
// that Headers values flow into legacy map[string]any signatures (and
// back) without conversion. This is the cornerstone of the "minimal
// caller migration" property of the Headers VO design.
func TestHeaders_AssignableToMapStringAny(t *testing.T) {
	h := messaging.Headers{"k": "v"}

	// Headers -> map[string]any (assignment, not conversion).
	var m map[string]any = h
	if m["k"] != "v" {
		t.Fatal("Headers should be assignable to map[string]any")
	}

	// map[string]any -> Headers (assignment).
	raw := map[string]any{"x": 1}
	var back messaging.Headers = raw
	if v, _ := back.Get("x"); v != 1 {
		t.Fatal("map[string]any should be assignable to Headers")
	}

	// Pass to a helper expecting map[string]any.
	if v, ok := messaging.GetHeaderString(h, "k"); !ok || v != "v" {
		t.Fatalf("legacy GetHeaderString(Headers): %q ok=%v", v, ok)
	}
}

package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// ═══════════════════════════════════════════════════════════════════
// Resolve & Address Rendering Audit Tests
//
// Validates issues identified by SEC-005, SEC-006:
//   - copyHeaders performs shallow copy (mutable reference sharing)
//   - RenderAddress control character injection
//   - StaticResolver defensive copy
//   - BindingResolver error paths
// ═══════════════════════════════════════════════════════════════════

// TestCopyHeaders_ShallowCopy validates that copyHeaders performs a
// shallow copy, so nested mutable values (slices, maps) share
// references between the copy and the original.
//
// ═══════════════════════════════════════════════════════════════════
// Bug: copyHeaders copies map entries by reference. If Options
// contains a []string or map[string]any, the DispatchPlan shares
// mutable references with the binding definition.
//
// binding.Options = map[string]any{"tags": []string{"a"}}
// plan.Headers["tags"].([]string)[0] = "modified"
//
//	→ binding.Options["tags"][0] == "modified"  (WRONG)
//
// ═══════════════════════════════════════════════════════════════════
func TestCopyHeaders_ShallowCopy_MutableSlice(t *testing.T) {
	original := map[string]any{
		"tags": []string{"original-tag"},
		"meta": map[string]any{"key": "val"},
	}

	copied := copyHeaders(original)

	copiedTags := copied["tags"].([]string)
	copiedTags[0] = "modified"

	origTags := original["tags"].([]string)
	if origTags[0] == "modified" {
		t.Fatal("copyHeaders shares []string reference with original (shallow copy bug)")
	}
}

// TestCopyHeaders_ShallowCopy_MutableMap validates that nested maps
// in copyHeaders are shared references.
func TestCopyHeaders_ShallowCopy_MutableMap(t *testing.T) {
	original := map[string]any{
		"meta": map[string]any{"key": "original"},
	}

	copied := copyHeaders(original)

	copiedMeta := copied["meta"].(map[string]any)
	copiedMeta["key"] = "modified"

	origMeta := original["meta"].(map[string]any)
	if origMeta["key"] == "modified" {
		t.Fatal("copyHeaders shares map[string]any reference with original (shallow copy bug)")
	}
}

// TestCopyHeaders_Nil validates that nil input returns nil.
func TestCopyHeaders_Nil(t *testing.T) {
	result := copyHeaders(nil)
	if result != nil {
		t.Fatal("copyHeaders(nil) should return nil")
	}
}

// TestCopyHeaders_Empty validates that empty map input returns nil.
func TestCopyHeaders_Empty(t *testing.T) {
	result := copyHeaders(map[string]any{})
	if result != nil {
		t.Fatal("copyHeaders(empty) should return nil")
	}
}

// TestRenderAddress_ControlCharInjection validates that substituted
// values containing control characters pass through (no sanitization).
// This documents the SEC-005 finding for non-MQTT transports.
func TestRenderAddress_ControlCharInjection(t *testing.T) {
	headers := map[string]any{
		"queue": "my-queue\x00injected",
	}

	result, err := RenderAddress("sqs://{queue}", headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "\x00") {
		t.Fatal("expected null byte to pass through (documenting SEC-005)")
	}
}

// TestRenderAddress_EmptyTemplate validates that empty template returns
// empty string without error.
func TestRenderAddress_EmptyTemplate(t *testing.T) {
	result, err := RenderAddress("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Fatalf("expected empty, got %q", result)
	}
}

// TestRenderAddress_MissingHeader validates that a missing header key
// returns an error.
func TestRenderAddress_MissingHeader(t *testing.T) {
	_, err := RenderAddress("topic/{missing}", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing header placeholder")
	}
}

// TestRenderAddress_RendersToEmpty validates that a template that
// renders to empty string returns an error.
func TestRenderAddress_RendersToEmpty(t *testing.T) {
	_, err := RenderAddress("{key}", map[string]any{"key": ""})
	if err == nil {
		t.Fatal("expected error for template that renders to empty string")
	}
}

// TestStaticResolver_ReturnsCopy_Audit validates that StaticResolver
// returns independent copies on each call.
func TestStaticResolver_ReturnsCopy_Audit(t *testing.T) {
	plans := []routing.DispatchPlan{
		{BindingID: "b1", Address: "topic/1"},
	}

	resolver := NewStaticResolver(plans...)

	result1, err := resolver.Resolve(context.Background(), &messaging.Envelope{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result1[0].Address = "mutated"

	result2, err := resolver.Resolve(context.Background(), &messaging.Envelope{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result2[0].Address == "mutated" {
		t.Fatal("StaticResolver returned shared slice, not a copy")
	}
}

// TestBindingResolver_NoMatch validates that resolver returns
// ErrNoBindingMatch when no binding matches.
func TestBindingResolver_NoMatch(t *testing.T) {
	resolver := NewBindingResolver(
		[]routing.DestinationBinding{{ID: "b1", Address: "topic"}},
		func(_ *messaging.Envelope, _ routing.DestinationBinding) bool { return false },
	)

	_, err := resolver.Resolve(context.Background(), &messaging.Envelope{})
	if err == nil {
		t.Fatal("expected error for no matching binding")
	}
}

// TestValidateMQTTTopic_LeadingSlash and TestValidateMQTTTopic_TrailingSlash
// were moved to adapters/mqtt/transport/paho/topic_validator_test.go as
// part of AP-005 — MQTT topic validation is no longer a runtime concern.

package runtime

// ═══════════════════════════════════════════════
// RenderAddress Security & Correctness Tests
//
// Tests validating fixes for:
// SEC-012: Infinite loop via self-referencing headers
// SEC-013: Template injection leaking header values
// GO-4: StaticResolver mutable slice return
//
// Summary:
// ┌──────┬────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                │ Status   │
// ├──────┼────────────────────────────────────────────┼──────────┤
// │ T001 │ Self-referencing header no infinite loop   │ PASS     │
// │ T002 │ Growing value no OOM                       │ PASS     │
// │ T003 │ Template injection prevented               │ PASS     │
// │ T004 │ Multiple placeholders in single template   │ PASS     │
// │ T005 │ Adjacent braces handled correctly          │ PASS     │
// │ T006 │ Empty placeholder returns error            │ PASS     │
// │ T007 │ Missing key returns error                  │ PASS     │
// │ T008 │ Literal braces in value not re-expanded    │ PASS     │
// │ T009 │ StaticResolver returns independent copy    │ PASS     │
// └──────┴────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════

import (
	"context"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// TestRenderAddress_SelfReference validates that a header value containing
// the same placeholder key does not cause an infinite loop (SEC-012).
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Template: "devices/{id}/cmd"
//	Header:   id = "{id}"
//	Old code: infinite loop (rescans from pos 0)
//	Fixed:    renders "devices/{id}/cmd" (literal braces in output)
//
// ───────────────────────────────────────────────
func TestRenderAddress_SelfReference(t *testing.T) {
	vars := map[string]any{"id": "{id}"}
	result, err := RenderAddress("devices/{id}/cmd", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "devices/{id}/cmd" {
		t.Fatalf("expected literal braces in output, got %q", result)
	}
}

// TestRenderAddress_GrowingValue validates no OOM from a value that
// grows the string on each substitution pass (SEC-012 variant).
func TestRenderAddress_GrowingValue(t *testing.T) {
	vars := map[string]any{"x": "grow{x}"}
	result, err := RenderAddress("{x}", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "grow{x}" {
		t.Fatalf("expected single expansion, got %q", result)
	}
}

// TestRenderAddress_TemplateInjection validates that substituted values
// cannot leak other header values (SEC-013).
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Template: "devices/{device_id}/commands"
//	Headers:  device_id = "{api_token}", api_token = "SECRET"
//	Old code: renders "devices/SECRET/commands" (leak!)
//	Fixed:    renders "devices/{api_token}/commands" (no leak)
//
// ───────────────────────────────────────────────
func TestRenderAddress_TemplateInjection(t *testing.T) {
	vars := map[string]any{
		"device_id": "{api_token}",
		"api_token": "SECRET-KEY-12345",
	}
	result, err := RenderAddress("devices/{device_id}/commands", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result, "SECRET") {
		t.Fatalf("template injection: secret leaked into result %q", result)
	}
	if result != "devices/{api_token}/commands" {
		t.Fatalf("expected literal {api_token}, got %q", result)
	}
}

// TestRenderAddress_MultiplePlaceholders validates correct handling of
// multiple placeholders in a single template.
func TestRenderAddress_MultiplePlaceholders(t *testing.T) {
	vars := map[string]any{
		"region":  "eu-west-1",
		"factory": "factory-A",
		"line":    "line-3",
	}
	result, err := RenderAddress("{region}/{factory}/{line}/orders", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "eu-west-1/factory-A/line-3/orders" {
		t.Fatalf("expected rendered address, got %q", result)
	}
}

// TestRenderAddress_AdjacentBraces validates handling of edge cases.
func TestRenderAddress_AdjacentBraces(t *testing.T) {
	tests := []struct {
		name     string
		template string
		vars     map[string]any
		want     string
		wantErr  bool
	}{
		{
			name:     "no placeholders",
			template: "static/path",
			vars:     nil,
			want:     "static/path",
		},
		{
			name:     "unclosed brace",
			template: "devices/{incomplete",
			vars:     nil,
			want:     "devices/{incomplete",
		},
		{
			name:     "empty template",
			template: "",
			vars:     nil,
			want:     "",
		},
		{
			name:     "empty placeholder",
			template: "devices/{}/cmd",
			vars:     nil,
			wantErr:  true,
		},
		{
			name:     "missing key",
			template: "devices/{unknown}/cmd",
			vars:     map[string]any{"other": "val"},
			wantErr:  true,
		},
		{
			name:     "value with braces not re-expanded",
			template: "path/{key}/end",
			vars:     map[string]any{"key": "val{nested}ue"},
			want:     "path/val{nested}ue/end",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := RenderAddress(tc.template, tc.vars)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tc.want {
				t.Fatalf("got %q, want %q", result, tc.want)
			}
		})
	}
}

// TestStaticResolver_ReturnsCopy validates that Resolve returns an
// independent copy, not the internal slice (GO-4).
func TestStaticResolver_ReturnsCopy(t *testing.T) {
	plans := []routing.DispatchPlan{
		{BindingID: "b1", Address: "addr1"},
		{BindingID: "b2", Address: "addr2"},
	}
	resolver := NewStaticResolver(plans...)

	result1, err := resolver.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	result1[0].Address = "MUTATED"

	result2, err := resolver.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if result2[0].Address == "MUTATED" {
		t.Fatal("StaticResolver returned mutable internal slice")
	}
	if result2[0].Address != "addr1" {
		t.Fatalf("expected addr1, got %s", result2[0].Address)
	}
}

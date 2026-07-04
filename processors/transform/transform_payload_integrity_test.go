package transform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func nextNoop(_ context.Context, _ *messaging.Envelope) error { return nil }

// ═══════════════════════════════════════════════════════════════════
// Payload integrity: when no payload mapping applies, the payload
// bytes must be byte-identical after Process — no key reordering, no
// HTML escaping, no {"_value": ...} wrapping of non-object payloads.
// ═══════════════════════════════════════════════════════════════════

// TestJSONTransform_HeaderOnlyMapping_PayloadBytesUntouched: a
// header-only transform must not rewrite the payload at all.
func TestJSONTransform_HeaderOnlyMapping_PayloadBytesUntouched(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{SimpleMapping("$.tenant", "header.x-tenant")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Deliberate key order, HTML-sensitive characters, and nesting: any
	// re-marshal would reorder keys and escape "<b>&".
	original := []byte(`{"z": 1, "tenant": "acme", "a": "<b>&", "nested": {"y": 2, "x": 1}}`)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: original})

	if err := proc.Process(context.Background(), env, nextNoop); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if got, ok := messaging.GetHeaderString(env.Headers(), "x-tenant"); !ok || got != "acme" {
		t.Fatalf("header x-tenant = %q (ok=%v), want acme", got, ok)
	}
	if !bytes.Equal(env.Payload(), original) {
		t.Fatalf("header-only transform rewrote the payload:\n got %s\nwant %s", env.Payload(), original)
	}
}

// TestJSONTransform_ArrayPayload_NoApplyingMapping_NotWrapped: an
// array payload with only header mappings (or non-matching payload
// mappings) must NOT be corrupted into {"_value": [...]}.
func TestJSONTransform_ArrayPayload_NoApplyingMapping_NotWrapped(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{
			SimpleMapping("$[1]", "header.x-second"),
			SimpleMapping("$.does.not.match", "unused"), // skipped: no match, no default
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	original := []byte(`[10, 20, 30]`)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: original})

	if err := proc.Process(context.Background(), env, nextNoop); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if !bytes.Equal(env.Payload(), original) {
		t.Fatalf("array payload corrupted:\n got %s\nwant %s", env.Payload(), original)
	}
	if got := env.Headers()["x-second"]; got != int64(20) && got != float64(20) {
		t.Fatalf("header x-second = %v (%T), want 20", got, got)
	}
}

// TestJSONTransform_AppliedMapping_NoHTMLEscaping: when the payload IS
// rewritten, serialization must not HTML-escape <, > and &.
func TestJSONTransform_AppliedMapping_NoHTMLEscaping(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{SimpleMapping("$.url", "link")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"url": "a&b<c>"}`)})
	if err := proc.Process(context.Background(), env, nextNoop); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if !bytes.Contains(env.Payload(), []byte(`"a&b<c>"`)) {
		t.Fatalf("transformed payload HTML-escaped: %s", env.Payload())
	}
}

// TestJSONTransform_ArrayPayload_ApplyingMapping_WrapsOnlyThen: the
// {"_value": ...} object wrap happens only when a keyed mapping target
// genuinely needs an object root on a non-object payload.
func TestJSONTransform_ArrayPayload_ApplyingMapping_WrapsOnlyThen(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{SimpleMapping("$[0]", "first")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`[10, 20]`)})
	if err := proc.Process(context.Background(), env, nextNoop); err != nil {
		t.Fatalf("Process: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(env.Payload(), &out); err != nil {
		t.Fatalf("unmarshal transformed payload: %v", err)
	}
	if out["first"] != float64(10) {
		t.Fatalf("first = %v, want 10", out["first"])
	}
	if _, ok := out["_value"]; !ok {
		t.Fatalf("expected original array preserved under _value, got %s", env.Payload())
	}
}

// TestJSONTransform_DropUnmapped_NoMappings_RewritesToEmptyObject:
// DropUnmapped=true is an explicit whole-payload rewrite — with no
// (matching) payload mappings the configured result is {}.
func TestJSONTransform_DropUnmapped_NoMappings_RewritesToEmptyObject(t *testing.T) {
	proc, err := New(Config{DropUnmapped: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"secret": "x"}`)})
	if err := proc.Process(context.Background(), env, nextNoop); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := string(env.Payload()); got != "{}" {
		t.Fatalf("DropUnmapped with no mappings: payload = %s, want {}", got)
	}
}

// ═══════════════════════════════════════════════════════════════════
// Mapping chains read from an immutable source snapshot in BOTH
// DropUnmapped modes.
// ═══════════════════════════════════════════════════════════════════

func TestJSONTransform_PayloadMappings_ReadImmutableSource(t *testing.T) {
	for _, dropUnmapped := range []bool{false, true} {
		name := "dropUnmapped=false"
		if dropUnmapped {
			name = "dropUnmapped=true"
		}
		t.Run(name, func(t *testing.T) {
			proc, err := New(Config{
				DropUnmapped: dropUnmapped,
				Mappings: []FieldMapping{
					// Overwrites "src" in the output first...
					SimpleMapping("$.other", "src"),
					// ...but this mapping must still read the ORIGINAL "src".
					SimpleMapping("$.src", "copy"),
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				Payload: []byte(`{"src": "ORIG", "other": "NEW"}`),
			})
			if err := proc.Process(context.Background(), env, nextNoop); err != nil {
				t.Fatalf("Process: %v", err)
			}

			var out map[string]any
			if err := json.Unmarshal(env.Payload(), &out); err != nil {
				t.Fatalf("unmarshal transformed payload: %v", err)
			}
			if out["src"] != "NEW" {
				t.Fatalf("src = %v, want overwritten NEW", out["src"])
			}
			if out["copy"] != "ORIG" {
				t.Fatalf("copy = %v, want ORIG (mapping read a mutated source)", out["copy"])
			}
		})
	}
}

// TestJSONTransform_NestedTargetWrite_DoesNotMutatePristineSource: a
// mapping that copies a submap and a later mapping whose nested target
// walks THROUGH that copy must never write into the pristine parsed
// source — a later mapping reading the source must not observe it.
func TestJSONTransform_NestedTargetWrite_DoesNotMutatePristineSource(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{
			// Copies the $.big submap into the output under "copy"...
			SimpleMapping("$.big", "copy"),
			// ...then writes through it via a nested target. If "copy"
			// aliases the pristine $.big, this write lands in the source.
			SimpleMapping("$.x", "copy.inner"),
			// Reads the pristine source AFTER the write above: must not
			// see "inner".
			SimpleMapping("$.big.inner", "leaked"),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Payload: []byte(`{"big": {"a": 1}, "x": 9}`),
	})
	if err := proc.Process(context.Background(), env, nextNoop); err != nil {
		t.Fatalf("Process: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(env.Payload(), &out); err != nil {
		t.Fatalf("unmarshal transformed payload: %v", err)
	}
	if v, leaked := out["leaked"]; leaked {
		t.Fatalf("pristine source mutated through aliased submap: $.big.inner = %v", v)
	}
	copyMap, ok := out["copy"].(map[string]any)
	if !ok {
		t.Fatalf("copy = %v (%T), want object", out["copy"], out["copy"])
	}
	if copyMap["inner"] != float64(9) {
		t.Fatalf("copy.inner = %v, want 9 (nested write must land in the output copy)", copyMap["inner"])
	}
}

// TestJSONTransform_DefaultValue_NotPoisonedAcrossMessages: a mapping
// whose DefaultValue is a container must hand each message a COPY. If
// the config-owned map itself is written into the output, a later
// nested-target write injects per-message data into the shared Config
// — poisoning it for every subsequent message (cross-message leakage).
func TestJSONTransform_DefaultValue_NotPoisonedAcrossMessages(t *testing.T) {
	def := map[string]any{"tier": "std"}
	proc, err := New(Config{
		Mappings: []FieldMapping{
			{Source: "$.missing", Target: "meta", DefaultValue: def},
			SimpleMapping("$.x", "meta.injected"),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	run := func(payload string) map[string]any {
		t.Helper()
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(payload)})
		if err := proc.Process(context.Background(), env, nextNoop); err != nil {
			t.Fatalf("Process: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(env.Payload(), &out); err != nil {
			t.Fatalf("unmarshal transformed payload: %v", err)
		}
		return out
	}

	run(`{"x": "msg1-secret"}`)
	if v, ok := def["injected"]; ok {
		t.Fatalf("config DefaultValue poisoned by message 1: injected = %v", v)
	}

	out2 := run(`{"y": 0}`)
	meta, ok := out2["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %v (%T), want object", out2["meta"], out2["meta"])
	}
	if v, ok := meta["injected"]; ok {
		t.Fatalf("message 2 sees message 1 data leaked via config DefaultValue: %v", v)
	}
}

// ═══════════════════════════════════════════════════════════════════
// TransformType validated at construction.
// ═══════════════════════════════════════════════════════════════════

func TestJSONTransform_UnknownTransformType_RejectedAtConstruction(t *testing.T) {
	_, err := New(Config{
		Mappings: []FieldMapping{{Source: "$.a", Target: "b", Transform: TransformType("sting")}},
	})
	if !errors.Is(err, ErrUnknownTransformType) {
		t.Fatalf("expected ErrUnknownTransformType, got %v", err)
	}
	if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorPermanent {
		t.Fatalf("expected permanent setup error, got %v", err)
	}

	// All supported types (including the empty default) still construct.
	for _, tt := range []TransformType{TransformNone, TransformString, TransformInt,
		TransformFloat, TransformBool, TransformBase64Encode, TransformBase64Decode} {
		if _, err := New(Config{
			Mappings: []FieldMapping{{Source: "$.a", Target: "b", Transform: tt}},
		}); err != nil {
			t.Fatalf("New with transform %q: %v", tt, err)
		}
	}
}

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

// TestJSONTransform_MappingFailure_IsRejectedNotRetryable is the L2 regression:
// a transform/mapping failure must surface as a rejected BridgeError so the
// runtime DLQs it. A plain fmt.Errorf would be treated as retryable, contrary
// to the documented "transform failures are permanent → DLQ" contract.
func TestJSONTransform_MappingFailure_IsRejectedNotRetryable(t *testing.T) {
	inputBytes, _ := json.Marshal(map[string]any{"other": "data"})

	tests := []struct {
		name    string
		mapping FieldMapping
	}{
		{"required field missing", RequiredMapping("$.nonexistent", "out")},
		{"failing type transform", TransformedMapping("$.other", "num", TransformInt)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proc, err := New(Config{FailOnError: true, Mappings: []FieldMapping{tc.mapping}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), inputBytes...)})
			err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
				return nil
			})

			if !errors.Is(err, shared.ErrInvalidPayload) {
				t.Fatalf("expected rejected ErrInvalidPayload, got %v", err)
			}
			if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorRejected {
				t.Fatalf("expected rejected BridgeError (DLQ-not-retry), got %v", err)
			}
			if shared.IsRecoverableError(err) {
				t.Fatal("transform failure must not be runtime-retryable")
			}
		})
	}
}

// TestJSONTransform_InvalidJSON is the L3 regression: an unparseable payload
// must not silently pass through when the configuration demands parsing
// (FailOnError or a Required mapping). The legitimate best-effort pass-through
// (FailOnError=false, no Required mapping) is preserved.
func TestJSONTransform_InvalidJSON(t *testing.T) {
	bad := []byte("not json {{{")

	t.Run("required mapping rejects unparseable payload", func(t *testing.T) {
		proc, err := New(Config{Mappings: []FieldMapping{RequiredMapping("$.id", "id")}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), bad...)})

		called := false
		err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
			called = true
			return nil
		})
		if called {
			t.Fatal("next must not be called for unparseable payload with a required mapping")
		}
		if !errors.Is(err, shared.ErrInvalidPayload) || shared.IsRecoverableError(err) {
			t.Fatalf("expected non-retryable ErrInvalidPayload, got %v", err)
		}
	})

	t.Run("FailOnError rejects unparseable payload", func(t *testing.T) {
		proc, err := New(Config{FailOnError: true, Mappings: []FieldMapping{SimpleMapping("$.id", "id")}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), bad...)})

		err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
			return nil
		})
		if !errors.Is(err, shared.ErrInvalidPayload) {
			t.Fatalf("expected ErrInvalidPayload, got %v", err)
		}
	})

	t.Run("best-effort passes unparseable payload through unchanged", func(t *testing.T) {
		proc, err := New(Config{Mappings: []FieldMapping{SimpleMapping("$.id", "id")}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), bad...)})

		called := false
		err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
			called = true
			return nil
		})
		if err != nil {
			t.Fatalf("expected pass-through success, got %v", err)
		}
		if !called {
			t.Fatal("next must be called on the best-effort pass-through path")
		}
		if !bytes.Equal(env.Payload(), bad) {
			t.Fatalf("payload must be unchanged on pass-through, got %q", env.Payload())
		}
	})
}

// TestJSONTransform_HeaderTarget_CopiesPayloadFieldToHeader is the L7 capability:
// a "header.<key>" target copies a parsed payload field into an envelope header,
// making the Scenario 14 payload-tenant-to-header recipe actually work.
func TestJSONTransform_HeaderTarget_CopiesPayloadFieldToHeader(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{SimpleMapping("$.tenant", "header.x-tenant")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inputBytes, _ := json.Marshal(map[string]any{"tenant": "acme"})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: inputBytes})

	err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	if got := env.Headers()["x-tenant"]; got != "acme" {
		t.Fatalf("expected header x-tenant=acme, got %v", got)
	}
}

// TestJSONTransform_Scenario14_PayloadTenantToHeader locks the exact
// docs/scenarios/14 recipe: a nested payload tenant field is copied into the
// non-reserved x-tenant header that the downstream resolver reads.
func TestJSONTransform_Scenario14_PayloadTenantToHeader(t *testing.T) {
	proc, err := New(Config{
		Name:     "extract-tenant",
		Mappings: []FieldMapping{{Source: "$.metadata.tenant_id", Target: "header.x-tenant"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inputBytes, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"tenant_id": "enterprise"},
		"priority": 9,
	})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: inputBytes})

	if err := proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Read it back the way the resolver's "header.x-tenant" condition would.
	got, ok := messaging.GetHeaderString(env.Headers(), "x-tenant")
	if !ok || got != "enterprise" {
		t.Fatalf("expected resolver-visible header x-tenant=enterprise, got %q (ok=%v)", got, ok)
	}
}

// TestJSONTransform_HeaderTarget_ReservedRejectedAtConstruction proves the
// fail-closed guard: a transform may not write a reserved x-bridge.* header
// (which the runtime strips at ingress) — rejected at construction.
func TestJSONTransform_HeaderTarget_ReservedRejectedAtConstruction(t *testing.T) {
	_, err := New(Config{
		Mappings: []FieldMapping{SimpleMapping("$.tenant", "header."+messaging.HeaderTenantID)},
	})
	if !errors.Is(err, ErrHeaderTargetReserved) {
		t.Fatalf("expected ErrHeaderTargetReserved, got %v", err)
	}
	if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorPermanent {
		t.Fatalf("expected permanent setup error, got %v", err)
	}
}

// TestJSONTransform_HeaderTarget_ReadsOriginalNotMutated is the L7 aliasing
// regression: with DropUnmapped=false the output map aliases the parsed
// payload, so a payload mapping that overwrites a field must NOT change what a
// later header mapping reads. Header targets always capture the ORIGINAL value,
// regardless of mapping order.
func TestJSONTransform_HeaderTarget_ReadsOriginalNotMutated(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{
			// Payload mapping FIRST: overwrites "src" with the value of "other".
			SimpleMapping("$.other", "src"),
			// Header mapping SECOND: reads "src" — must see the ORIGINAL value.
			SimpleMapping("$.src", "header.x-copy"),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inputBytes, _ := json.Marshal(map[string]any{"src": "ORIG", "other": "NEW"})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: inputBytes})

	if err := proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Header captured the original value despite the payload overwrite.
	if got, ok := messaging.GetHeaderString(env.Headers(), "x-copy"); !ok || got != "ORIG" {
		t.Fatalf("expected header x-copy=ORIG (original, pre-mutation), got %q (ok=%v)", got, ok)
	}
	// And the payload overwrite still took effect.
	var out map[string]any
	if err := json.Unmarshal(env.Payload(), &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if out["src"] != "NEW" {
		t.Fatalf("expected payload src overwritten to NEW, got %v", out["src"])
	}
}

// TestJSONTransform_HeaderTargetEmpty_RejectedAtConstruction proves a bare
// "header." target (no key) is rejected at construction as a permanent setup
// error rather than silently writing an empty-named header per message.
func TestJSONTransform_HeaderTargetEmpty_RejectedAtConstruction(t *testing.T) {
	_, err := New(Config{
		Mappings: []FieldMapping{SimpleMapping("$.tenant", "header.")},
	})
	if !errors.Is(err, ErrHeaderTargetEmpty) {
		t.Fatalf("expected ErrHeaderTargetEmpty, got %v", err)
	}
	if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorPermanent {
		t.Fatalf("expected permanent setup error, got %v", err)
	}
}

// TestJSONTransform_CancelledContext_AbortsWithoutMutating is the L9 regression:
// once the context is cancelled the processor returns a transient timeout and
// mutates neither the payload nor the headers (the runtime may have moved on).
func TestJSONTransform_CancelledContext_AbortsWithoutMutating(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{
			SimpleMapping("$.tenant", "header.x-tenant"),
			SimpleMapping("$.tenant", "tenant_copy"),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inputBytes, _ := json.Marshal(map[string]any{"tenant": "acme"})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), inputBytes...)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	err = proc.Process(ctx, env, func(_ context.Context, _ *messaging.Envelope) error {
		called = true
		return nil
	})
	if called {
		t.Fatal("next must not be called after cancellation")
	}
	if _, ok := env.Headers()["x-tenant"]; ok {
		t.Fatal("headers must not be mutated after cancellation")
	}
	if !bytes.Equal(env.Payload(), inputBytes) {
		t.Fatal("payload must not be mutated after cancellation")
	}
	if !errors.Is(err, shared.ErrProcessorTimeout) || !shared.IsRecoverableError(err) {
		t.Fatalf("expected transient ErrProcessorTimeout, got %v", err)
	}
}

// TestJSONTransform_OversizedPayload_Rejected is the L9 CPU-bound guard: a
// payload above MaxPayloadBytes is rejected before parsing and never reaches next.
func TestJSONTransform_OversizedPayload_Rejected(t *testing.T) {
	proc, err := New(Config{
		MaxPayloadBytes: 4,
		Mappings:        []FieldMapping{SimpleMapping("$.id", "id")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"id":"0123456789"}`)})

	called := false
	err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		called = true
		return nil
	})
	if called {
		t.Fatal("oversized payload must not reach next")
	}
	if !errors.Is(err, shared.ErrPayloadTooLarge) || shared.IsRecoverableError(err) {
		t.Fatalf("expected non-retryable ErrPayloadTooLarge, got %v", err)
	}
}

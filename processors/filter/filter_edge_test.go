package filter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestFilter_UnknownAction_RejectedAtConstruction validates that an Action
// outside {pass, drop, route} is rejected by New (fail fast at build) rather
// than silently falling through to next per message — a misconfigured policy
// gate must not degrade into a no-op.
func TestFilter_UnknownAction_RejectedAtConstruction(t *testing.T) {
	tests := []struct {
		name   string
		action Action
	}{
		{"custom action", Action("custom")},
		{"empty action", Action("")},
		{"unknown action", Action("unknown")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				Name:   "unknown-action",
				Action: tc.action,
				Conditions: []Condition{
					{Field: "subject", Operator: OperatorEquals, Value: "test"},
				},
			})
			if !errors.Is(err, ErrUnknownAction) {
				t.Fatalf("expected ErrUnknownAction at construction, got %v", err)
			}
			if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorPermanent {
				t.Fatalf("expected a permanent BridgeError setup error, got %v", err)
			}
		})
	}
}

// TestFilter_ReDoS_GoRegexpSafe validates that Go's RE2-based regexp engine
// prevents catastrophic backtracking with pathological patterns.
func TestFilter_ReDoS_GoRegexpSafe(t *testing.T) {
	p, err := NewDropFilter("redos-test",
		Condition{
			Field:    "subject",
			Operator: OperatorRegex,
			Value:    `(a+)+b`,
		},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	input := strings.Repeat("a", 16) + "!"
	env := envelope(input, nil, nil)

	start := time.Now()
	err = p.Process(context.Background(), env, nextOK)
	elapsed := time.Since(start)

	if errors.Is(err, shared.ErrMessageFiltered) {
		t.Fatal("pattern should not match input ending with '!'")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("regex evaluation took %v; RE2 should prevent catastrophic backtracking", elapsed)
	}
}

// TestFilter_CancelledContext_AbortsWithoutMutating is the L9 regression: once
// the runtime cancels the context the processor must stop promptly, return a
// transient (retryable) timeout error, call neither next nor mutate the
// envelope (which the runtime may already have moved on).
func TestFilter_CancelledContext_AbortsWithoutMutating(t *testing.T) {
	p, err := NewRouteFilter("route-cancel", "other-route",
		Condition{Field: "subject", Operator: OperatorEquals, Value: "test"},
	)
	if err != nil {
		t.Fatalf("NewRouteFilter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // runtime already timed out / moved on

	env := envelope("test", nil, nil)
	called := false
	err = p.Process(ctx, env, func(_ context.Context, _ *messaging.Envelope) error {
		called = true
		return nil
	})

	if called {
		t.Fatal("next must not be called after context cancellation")
	}
	if _, ok := env.Headers()[messaging.HeaderRouteOverride]; ok {
		t.Fatal("envelope must not be mutated after context cancellation")
	}
	if !errors.Is(err, shared.ErrProcessorTimeout) {
		t.Fatalf("expected transient ErrProcessorTimeout, got %v", err)
	}
	if !shared.IsRecoverableError(err) {
		t.Fatal("cancellation must be runtime-retryable (transient)")
	}
}

// TestFilter_OversizedPayload_Rejected is the L9 regression bounding worst-case
// parse CPU: a payload above MaxPayloadBytes is rejected (DLQ-not-retry) before
// the JSON parser runs, and never reaches next.
func TestFilter_OversizedPayload_Rejected(t *testing.T) {
	p, err := New(Config{
		Name:            "oversized",
		Action:          ActionDrop,
		MaxPayloadBytes: 4,
		Conditions: []Condition{
			{Field: "$.field", Operator: OperatorEquals, Value: "value"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "test",
		Payload: []byte(`{"field":"value"}`),
	})

	called := false
	err = p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		called = true
		return nil
	})
	if called {
		t.Fatal("oversized payload must not reach next")
	}
	if !errors.Is(err, shared.ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
	if shared.IsRecoverableError(err) {
		t.Fatal("payload-too-large reject must not be runtime-retryable")
	}
}

// TestCondition_MalformedJSON_DoesNotBypassDropFilter is the L5 security
// regression: a malformed JSON payload must NOT silently no-match its way past
// an ActionDrop (deny) rule. Fail closed — Process returns a rejected
// BridgeError (so the runtime DLQs it) and never calls next.
func TestCondition_MalformedJSON_DoesNotBypassDropFilter(t *testing.T) {
	p, err := NewDropFilter("invalid-json",
		Condition{
			Field:    "$.field",
			Operator: OperatorEquals,
			Value:    "value",
		},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "test",
		Payload: []byte("not json {{{"),
	})

	called := false
	err = p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		called = true
		return nil
	})
	if called {
		t.Fatal("SECURITY: malformed JSON bypassed the drop filter (next was called)")
	}
	if !errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatalf("expected rejected ErrInvalidPayload, got %v", err)
	}
	if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorRejected {
		t.Fatalf("expected a rejected BridgeError (DLQ-not-retry), got %v", err)
	}
	if shared.IsRecoverableError(err) {
		t.Fatal("malformed-JSON reject must not be runtime-retryable")
	}
}

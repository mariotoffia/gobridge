package filter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestFilter_UnknownAction_FallsThroughToNext validates that when Action is an
// undefined value, the default case in Process calls next(ctx, env).
func TestFilter_UnknownAction_FallsThroughToNext(t *testing.T) {
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
			p, err := New(Config{
				Name:   "unknown-action",
				Action: tc.action,
				Conditions: []Condition{
					{Field: "subject", Operator: OperatorEquals, Value: "test"},
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			called := false
			env := envelope("test", nil, nil)
			err = p.Process(context.Background(), env, func(_ context.Context, _ *domain.Envelope) error {
				called = true
				return nil
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !called {
				t.Error("expected next to be called for unknown action")
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

// TestCondition_InvalidJSONPayload_SilentNoMatch validates that invalid JSON
// in payload results in silent no-match rather than an error.
func TestCondition_InvalidJSONPayload_SilentNoMatch(t *testing.T) {
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

	env := &domain.Envelope{
		Subject: "test",
		Payload: []byte("not json {{{"),
	}

	called := false
	err = p.Process(context.Background(), env, func(_ context.Context, _ *domain.Envelope) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil (silent no-match), got %v", err)
	}
	if !called {
		t.Error("expected next to be called when payload is invalid JSON (no match)")
	}
}

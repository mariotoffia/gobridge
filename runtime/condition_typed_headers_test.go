package runtime

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestConditionEval_TypedHeaderAccessor_HeaderDotPrefix proves the M-2
// migration: extractField goes through messaging.Headers.Get() rather
// than direct map indexing for the "header.<name>" form, including the
// nil-headers branch (no panic, no match).
func TestConditionEval_TypedHeaderAccessor_HeaderDotPrefix(t *testing.T) {
	eval, err := newConditionEval(MatchCondition{
		Field: "header.x-tenant", Operator: OpEquals, Value: Val("acme"),
	})
	if err != nil {
		t.Fatalf("newConditionEval: %v", err)
	}

	// Hit case.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{"x-tenant": "acme"},
	})
	if ok, err := eval.evaluate(env, newEvalContext()); err != nil || !ok {
		t.Fatalf("expected typed Get hit; ok=%v err=%v", ok, err)
	}

	// Miss case (key absent).
	envMiss := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{"other": "v"},
	})
	if ok, _ := eval.evaluate(envMiss, newEvalContext()); ok {
		t.Fatal("expected miss when key absent")
	}

	// Nil-headers case must not panic and must return false.
	envNil := &messaging.Envelope{}
	if ok, err := eval.evaluate(envNil, newEvalContext()); err != nil || ok {
		t.Fatalf("nil-headers branch: ok=%v err=%v", ok, err)
	}
}

// TestConditionEval_TypedHeaderAccessor_BareFieldFallback proves the
// bare-field fallback path also routes through Headers.Get().
func TestConditionEval_TypedHeaderAccessor_BareFieldFallback(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{
		Field: "tenant", Operator: OpEquals, Value: Val("acme"),
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{"tenant": "acme"},
	})
	if ok, err := eval.evaluate(env, newEvalContext()); err != nil || !ok {
		t.Fatalf("expected bare-field hit; ok=%v err=%v", ok, err)
	}

	// Nil-headers branch on the default arm.
	envNil := &messaging.Envelope{}
	if ok, _ := eval.evaluate(envNil, newEvalContext()); ok {
		t.Fatal("nil headers should not match bare-field condition")
	}
}

// TestConditionEval_TypedHeaderAccessor_ExistsOnEmptyHeaders verifies
// that the OpExists operator composes with the typed-VO nil-safe Get,
// reporting "not exists" without panicking on a zero-value envelope.
func TestConditionEval_TypedHeaderAccessor_ExistsOnEmptyHeaders(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{
		Field: "header.x", Operator: OpExists, Value: Val(false),
	})
	env := &messaging.Envelope{}
	if ok, err := eval.evaluate(env, newEvalContext()); err != nil || !ok {
		t.Fatalf("expected exists=false to be true on empty headers; ok=%v err=%v", ok, err)
	}
}

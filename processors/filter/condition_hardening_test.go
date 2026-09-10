package filter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ═══════════════════════════════════════════════════════════════════
// Numeric coercion for eq / ne / in
//
// JSON payload numbers decode as float64 while config values are
// typically Go ints. Without coercion a deny filter {eq, 3} never
// matched {"priority": 3} and FAILED OPEN. Equality must be exact:
// integers above 2^53 must not collapse onto nearby floats.
// ═══════════════════════════════════════════════════════════════════

func TestLooseEqual_NumericCoercion(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		// The failure mode that motivated the fix: float64 (JSON) vs int (config).
		{"float64 vs int equal", float64(3), int(3), true},
		{"int vs float64 equal", int(3), float64(3), true},
		{"float64 vs int not equal", float64(3), int(4), false},
		{"float64 fraction vs int", float64(3.5), int(3), false},

		// All int/float combos.
		{"int vs int64", int(7), int64(7), true},
		{"int32 vs float64", int32(-2), float64(-2), true},
		{"float32 vs int", float32(8), int(8), true},
		{"float64 vs float32", float64(1.5), float32(1.5), true},
		{"uint vs int", uint(9), int(9), true},
		{"uint64 vs float64", uint64(10), float64(10), true},
		{"negative int vs uint", int(-1), uint64(0xFFFFFFFFFFFFFFFF), false},
		{"json.Number int vs int", json.Number("42"), int(42), true},
		{"json.Number float vs float64", json.Number("2.5"), float64(2.5), true},

		// Precision above 2^53: exact integer comparison required.
		{"int64 2^53+1 vs float64 2^53", int64(1<<53 + 1), float64(1 << 53), false},
		{"int64 2^53 vs float64 2^53", int64(1 << 53), float64(1 << 53), true},
		{"int64 max vs float64 2^63", int64(1<<63 - 1), maxInt64AsFloat, false},
		{"uint64 max vs float64 2^64", uint64(1<<64 - 1), maxUint64AsFloat, false},

		// Non-numeric pairs keep strict semantics: no string↔number coercion.
		{"string 3 vs int 3", "3", int(3), false},
		{"string vs string equal", "x", "x", true},
		{"bool vs int", true, int(1), false},
		{"nil vs zero", nil, int(0), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looseEqual(tt.a, tt.b); got != tt.want {
				t.Fatalf("looseEqual(%v (%T), %v (%T)) = %v, want %v",
					tt.a, tt.a, tt.b, tt.b, got, tt.want)
			}
			// Equality is symmetric.
			if got := looseEqual(tt.b, tt.a); got != tt.want {
				t.Fatalf("looseEqual(%v (%T), %v (%T)) = %v, want %v (symmetry)",
					tt.b, tt.b, tt.a, tt.a, got, tt.want)
			}
		})
	}
}

// TestCondition_Equals_NumericPayload drives eq/ne/in through the
// evaluator against a real JSON payload (numbers decode as float64).
func TestCondition_Equals_NumericPayload(t *testing.T) {
	payload := []byte(`{"priority": 3, "score": 2.5}`)

	tests := []struct {
		name string
		cond Condition
		want bool
	}{
		{"eq int config vs float payload", Condition{Field: "$.priority", Operator: OperatorEquals, Value: 3}, true},
		{"eq mismatch", Condition{Field: "$.priority", Operator: OperatorEquals, Value: 4}, false},
		{"ne int config vs float payload", Condition{Field: "$.priority", Operator: OperatorNotEquals, Value: 3}, false},
		{"ne mismatch is true", Condition{Field: "$.priority", Operator: OperatorNotEquals, Value: 4}, true},
		{"in with int members", Condition{Field: "$.priority", Operator: OperatorIn, Value: []any{1, 2, 3}}, true},
		{"in without match", Condition{Field: "$.priority", Operator: OperatorIn, Value: []any{4, 5}}, false},
		{"eq float config vs float payload", Condition{Field: "$.score", Operator: OperatorEquals, Value: 2.5}, true},
		{"in with float member", Condition{Field: "$.score", Operator: OperatorIn, Value: []any{2.5}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval, err := newConditionEvaluator(tt.cond)
			if err != nil {
				t.Fatalf("newConditionEvaluator: %v", err)
			}
			got, err := eval.evaluate(envelope("", nil, json.RawMessage(payload)))
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if got != tt.want {
				t.Fatalf("evaluate = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDropFilter_NumericEq_DropsMessage is the policy-level regression:
// a deny rule {eq, 3} on a numeric JSON field must DROP the message,
// not forward it.
func TestDropFilter_NumericEq_DropsMessage(t *testing.T) {
	proc, err := NewDropFilter("drop-priority-3",
		Condition{Field: "$.priority", Operator: OperatorEquals, Value: 3})
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := envelope("orders", nil, json.RawMessage(`{"priority": 3}`))
	nextCalled := false
	err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		nextCalled = true
		return nil
	})
	if !errors.Is(err, shared.ErrMessageFiltered) {
		t.Fatalf("expected ErrMessageFiltered, got %v", err)
	}
	if nextCalled {
		t.Fatal("deny filter failed open: next was called for a matching message")
	}
}

// ═══════════════════════════════════════════════════════════════════
// gt/lt on a non-numeric field must be a rejected (DLQ) error, not a
// bare transient one that redelivers the same message forever.
// ═══════════════════════════════════════════════════════════════════

func TestCondition_NumericCompare_NonNumericField_IsRejected(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "$.status", Operator: OperatorGreaterThan, Value: 3,
	})
	if err != nil {
		t.Fatalf("newConditionEvaluator: %v", err)
	}

	_, err = eval.evaluate(envelope("", nil, json.RawMessage(`{"status": "shipped"}`)))
	if err == nil {
		t.Fatal("expected error for non-numeric field in gt comparison")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T: %v", err, err)
	}
	if be.Code != shared.ErrCodeInvalidPayload || be.Class != shared.ErrorRejected {
		t.Fatalf("expected rejected INVALID_PAYLOAD (DLQ, not retry), got code=%s class=%s", be.Code, be.Class)
	}
	if shared.IsRecoverableError(err) {
		t.Fatal("non-numeric-field comparison error must not be classified recoverable (uncapped retry loop)")
	}
}

// TestCondition_NumericValue_ValidatedAtConstruction: a non-numeric
// CONFIG value for an ordering operator is a deterministic
// misconfiguration and fails the build.
func TestCondition_NumericValue_ValidatedAtConstruction(t *testing.T) {
	for _, op := range []Operator{OperatorGreaterThan, OperatorLessThan, OperatorGreaterOrEqual, OperatorLessOrEqual} {
		t.Run(string(op), func(t *testing.T) {
			_, err := newConditionEvaluator(Condition{Field: "$.x", Operator: op, Value: []any{1}})
			if !errors.Is(err, ErrComparisonValueNotNumeric) {
				t.Fatalf("expected ErrComparisonValueNotNumeric, got %v", err)
			}
		})
	}

	// Numeric strings remain accepted (pre-existing toFloat64 contract).
	if _, err := newConditionEvaluator(Condition{Field: "$.x", Operator: OperatorGreaterThan, Value: "3.5"}); err != nil {
		t.Fatalf("numeric string comparison value rejected: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════
// exists requires an explicit bool value at construction.
// ═══════════════════════════════════════════════════════════════════

func TestCondition_Exists_RequiresExplicitBool(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{"missing value", nil},
		{"string value", "true"},
		{"int value", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newConditionEvaluator(Condition{
				Field: "header.x", Operator: OperatorExists, Value: tt.value,
			})
			if !errors.Is(err, ErrExistsValueNotBool) {
				t.Fatalf("expected ErrExistsValueNotBool, got %v", err)
			}
			if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorPermanent {
				t.Fatalf("expected permanent setup error, got %v", err)
			}
		})
	}

	// New surfaces the same validation for a whole filter config.
	_, err := New(Config{
		Action:     ActionDrop,
		Conditions: []Condition{{Field: "header.x", Operator: OperatorExists}},
	})
	if !errors.Is(err, ErrExistsValueNotBool) {
		t.Fatalf("New: expected ErrExistsValueNotBool, got %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════
// Payload is parsed once per Process call and shared read-only.
// ═══════════════════════════════════════════════════════════════════

// TestPayloadDoc_ParsesOnce: the second get returns the cached document
// (same map instance), proving the payload is not re-copied/re-parsed
// per condition.
func TestPayloadDoc_ParsesOnce(t *testing.T) {
	env := envelope("", nil, json.RawMessage(`{"a": 1, "b": 2}`))
	doc := &payloadDoc{}

	m1, err := doc.get(env, 0, "$.a")
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	m2, err := doc.get(env, 0, "$.b")
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if reflect.ValueOf(m1).Pointer() != reflect.ValueOf(m2).Pointer() {
		t.Fatal("expected the same parsed document instance on the second get (parse-once)")
	}
}

// TestPayloadDoc_CachesParseFailure: a malformed payload fails closed
// once and the cached classification is returned to every subsequent
// condition without re-parsing.
func TestPayloadDoc_CachesParseFailure(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s"})
	env.SetPayload([]byte(`{not-json`))
	doc := &payloadDoc{}

	_, err1 := doc.get(env, 0, "$.a")
	if !errors.Is(err1, shared.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err1)
	}
	_, err2 := doc.get(env, 0, "$.b")
	if !errors.Is(err2, shared.ErrInvalidPayload) {
		t.Fatalf("expected cached ErrInvalidPayload, got %v", err2)
	}
}

// TestFilter_MultiplePayloadConditions_SingleParse: end-to-end — a
// filter with several "$." conditions evaluates them all against one
// shared parsed document and still gets the right answer.
func TestFilter_MultiplePayloadConditions_SingleParse(t *testing.T) {
	proc, err := NewDropFilter("multi",
		Condition{Field: "$.priority", Operator: OperatorEquals, Value: 3},
		Condition{Field: "$.region", Operator: OperatorEquals, Value: "eu"},
		Condition{Field: "$.retries", Operator: OperatorLessThan, Value: 5},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := envelope("orders", nil, json.RawMessage(`{"priority": 3, "region": "eu", "retries": 1}`))
	err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error { return nil })
	if !errors.Is(err, shared.ErrMessageFiltered) {
		t.Fatalf("expected all-conditions match to drop, got %v", err)
	}
}

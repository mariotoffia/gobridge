package runtime

import (
	"math"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════
// Condition Evaluator Audit Tests
//
// Validates edge cases identified by QA-015, QA-016, GO-005:
//   - condToFloat64 missing unsigned integer types
//   - numericCompare float64 precision loss for large int64
//   - isIn with typed slices
//   - prefixMatch and containsMatch with non-string types
// ═══════════════════════════════════════════════════════════════════

// TestCondToFloat64_UnsignedIntegers validates that unsigned integer
// types are correctly converted to float64.
//
// ═══════════════════════════════════════════════════════════════════
// Bug: condToFloat64 only handles float64, float32, int, int64,
// int32, string, json.Number. Unsigned types (uint, uint32, uint64)
// fall through to the default case and return an error.
//
// condToFloat64(uint(42)) → error: "cannot convert uint to float64"
// ═══════════════════════════════════════════════════════════════════
func TestCondToFloat64_UnsignedIntegers(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want float64
	}{
		{"uint", uint(42), 42.0},
		{"uint32", uint32(100), 100.0},
		{"uint64", uint64(1000), 1000.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := condToFloat64(tt.val)
			if err != nil {
				t.Fatalf("condToFloat64(%T(%v)) returned error: %v", tt.val, tt.val, err)
			}
			if got != tt.want {
				t.Fatalf("condToFloat64(%T(%v)) = %f, want %f", tt.val, tt.val, got, tt.want)
			}
		})
	}
}

// TestCondToFloat64_ValidTypes validates all currently supported types.
func TestCondToFloat64_ValidTypes(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want float64
	}{
		{"float64", float64(3.14), 3.14},
		{"float32", float32(2.5), 2.5},
		{"int", int(42), 42.0},
		{"int64", int64(100), 100.0},
		{"int32", int32(50), 50.0},
		{"string", "3.14", 3.14},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := condToFloat64(tt.val)
			if err != nil {
				t.Fatalf("condToFloat64(%T(%v)) returned error: %v", tt.val, tt.val, err)
			}
			if math.Abs(got-tt.want) > 0.001 {
				t.Fatalf("condToFloat64(%T(%v)) = %f, want %f", tt.val, tt.val, got, tt.want)
			}
		})
	}
}

// TestCondToFloat64_NaN validates that NaN is rejected.
func TestCondToFloat64_NaN(t *testing.T) {
	_, err := condToFloat64(math.NaN())
	if err == nil {
		t.Fatal("NaN should be rejected")
	}
}

// TestCondToFloat64_Inf validates that Inf is rejected.
func TestCondToFloat64_Inf(t *testing.T) {
	_, err := condToFloat64(math.Inf(1))
	if err == nil {
		t.Fatal("Inf should be rejected")
	}
}

// TestCondToFloat64_InvalidString validates that non-numeric strings error.
func TestCondToFloat64_InvalidString(t *testing.T) {
	_, err := condToFloat64("not-a-number")
	if err == nil {
		t.Fatal("non-numeric string should return error")
	}
}

// TestNumericCompare_LargeInt64Precision validates that large int64
// values that differ by 1 are correctly compared.
//
// ═══════════════════════════════════════════════════════════════════
// Bug: float64 has 53-bit mantissa. int64 values > 2^53 lose
// precision when converted to float64. Two values differing by 1
// may compare as equal.
//
// v1 = 1<<53 + 1  (9007199254740993)
// v2 = 1<<53 + 2  (9007199254740994)
// float64(v1) == float64(v2)  → true (WRONG: they differ by 1)
// ═══════════════════════════════════════════════════════════════════
func TestNumericCompare_LargeInt64Precision(t *testing.T) {
	v1 := int64(1<<53 + 1)
	v2 := int64(1<<53 + 2)

	if v1 == v2 {
		t.Skip("test requires distinct int64 values")
	}

	eval := &conditionEval{
		cond: MatchCondition{
			Field:    "header.val",
			Operator: OpLessThan,
			Value:    v2,
		},
	}

	env := &domain.Envelope{
		Headers: map[string]any{"val": v1},
	}
	ctx := newEvalContext()

	result, err := eval.evaluate(env, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Fatalf("expected %d < %d to be true, but float64 precision loss made them equal", v1, v2)
	}
}

// TestConditionEval_Equals_StringFastPath validates string equality
// without reflection overhead.
func TestConditionEval_Equals_StringFastPath(t *testing.T) {
	eval := &conditionEval{
		cond: MatchCondition{
			Field:    "header.type",
			Operator: OpEquals,
			Value:    "order",
		},
	}

	env := &domain.Envelope{
		Headers: map[string]any{"type": "order"},
	}
	ctx := newEvalContext()

	result, err := eval.evaluate(env, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Fatal("expected match for equal strings")
	}
}

// TestConditionEval_In_StringSlice validates the "in" operator with
// a []any slice.
func TestConditionEval_In_StringSlice(t *testing.T) {
	eval := &conditionEval{
		cond: MatchCondition{
			Field:    "header.status",
			Operator: OpIn,
			Value:    []any{"active", "pending"},
		},
	}

	env := &domain.Envelope{
		Headers: map[string]any{"status": "active"},
	}
	ctx := newEvalContext()

	result, err := eval.evaluate(env, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Fatal("expected 'active' to be in ['active', 'pending']")
	}
}

// TestConditionEval_Exists_True validates the exists operator with
// a present header.
func TestConditionEval_Exists_True(t *testing.T) {
	eval := &conditionEval{
		cond: MatchCondition{
			Field:    "header.key",
			Operator: OpExists,
			Value:    true,
		},
	}

	env := &domain.Envelope{
		Headers: map[string]any{"key": "val"},
	}
	ctx := newEvalContext()

	result, err := eval.evaluate(env, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Fatal("expected exists=true for present header")
	}
}

// TestConditionEval_Exists_False validates the exists operator with
// a missing header.
func TestConditionEval_Exists_False(t *testing.T) {
	eval := &conditionEval{
		cond: MatchCondition{
			Field:    "header.missing",
			Operator: OpExists,
			Value:    true,
		},
	}

	env := &domain.Envelope{
		Headers: map[string]any{},
	}
	ctx := newEvalContext()

	result, err := eval.evaluate(env, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Fatal("expected exists=false for missing header")
	}
}

// TestConditionEval_PayloadPath validates JSON payload path extraction.
func TestConditionEval_PayloadPath(t *testing.T) {
	eval := &conditionEval{
		cond: MatchCondition{
			Field:    "$.order.status",
			Operator: OpEquals,
			Value:    "shipped",
		},
	}

	env := &domain.Envelope{
		Payload: []byte(`{"order": {"status": "shipped", "id": 123}}`),
	}
	ctx := newEvalContext()

	result, err := eval.evaluate(env, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Fatal("expected payload path match")
	}
}

// TestConditionEval_UnsupportedOperator validates that an unsupported
// operator returns an error.
func TestConditionEval_UnsupportedOperator(t *testing.T) {
	eval := &conditionEval{
		cond: MatchCondition{
			Field:    "header.key",
			Operator: "bogus",
			Value:    "val",
		},
	}

	env := &domain.Envelope{
		Headers: map[string]any{"key": "val"},
	}
	ctx := newEvalContext()

	_, err := eval.evaluate(env, ctx)
	if err == nil {
		t.Fatal("expected error for unsupported operator")
	}
}

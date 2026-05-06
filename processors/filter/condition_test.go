package filter

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

func TestCondition_Equals(t *testing.T) {
	tests := []struct {
		name   string
		cond   Condition
		env    *messaging.Envelope
		expect bool
	}{
		{
			name:   "subject equals match",
			cond:   Condition{Field: "subject", Operator: OperatorEquals, Value: "orders"},
			env:    envelope("orders", nil, nil),
			expect: true,
		},
		{
			name:   "subject equals mismatch",
			cond:   Condition{Field: "subject", Operator: OperatorEquals, Value: "events"},
			env:    envelope("orders", nil, nil),
			expect: false,
		},
		{
			name:   "header equals match",
			cond:   Condition{Field: "header.type", Operator: OperatorEquals, Value: "order.created"},
			env:    envelope("", map[string]any{"type": "order.created"}, nil),
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval, err := newConditionEvaluator(tt.cond)
			if err != nil {
				t.Fatalf("unexpected evaluator error: %v", err)
			}
			result, err := eval.evaluate(tt.env)
			if err != nil {
				t.Fatalf("unexpected evaluate error: %v", err)
			}
			if result != tt.expect {
				t.Fatalf("expected %v, got %v", tt.expect, result)
			}
		})
	}
}

func TestCondition_NotEquals(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "subject", Operator: OperatorNotEquals, Value: "skip",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("orders", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !result {
		t.Fatal("expected true for not-equals mismatch")
	}
}

func TestCondition_Contains(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "subject", Operator: OperatorContains, Value: "ord",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("orders.created", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !result {
		t.Fatal("expected true for contains match")
	}
}

func TestCondition_Regex(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "subject", Operator: OperatorRegex, Value: `^order\.\w+$`,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("order.created", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !result {
		t.Fatal("expected regex match")
	}
}

func TestCondition_Regex_InvalidPattern(t *testing.T) {
	_, err := newConditionEvaluator(Condition{
		Field: "subject", Operator: OperatorRegex, Value: `[invalid`,
	})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestCondition_Regex_NonStringValue(t *testing.T) {
	_, err := newConditionEvaluator(Condition{
		Field: "subject", Operator: OperatorRegex, Value: 42,
	})
	if err == nil {
		t.Fatal("expected error for non-string regex value")
	}
}

func TestCondition_NumericComparisons(t *testing.T) {
	tests := []struct {
		name   string
		op     Operator
		val    any
		hdrVal any
		expect bool
	}{
		{"gt true", OperatorGreaterThan, float64(5), float64(10), true},
		{"gt false", OperatorGreaterThan, float64(10), float64(5), false},
		{"lt true", OperatorLessThan, float64(10), float64(5), true},
		{"lt false", OperatorLessThan, float64(5), float64(10), false},
		{"gte equal", OperatorGreaterOrEqual, float64(5), float64(5), true},
		{"gte greater", OperatorGreaterOrEqual, float64(5), float64(10), true},
		{"lte equal", OperatorLessOrEqual, float64(5), float64(5), true},
		{"lte less", OperatorLessOrEqual, float64(10), float64(5), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval, err := newConditionEvaluator(Condition{
				Field: "header.count", Operator: tt.op, Value: tt.val,
			})
			if err != nil {
				t.Fatal(err)
			}
			env := envelope("", map[string]any{"count": tt.hdrVal}, nil)
			result, err := eval.evaluate(env)
			if err != nil {
				t.Fatal(err)
			}
			if result != tt.expect {
				t.Fatalf("expected %v, got %v", tt.expect, result)
			}
		})
	}
}

func TestCondition_NumericCompare_NonNumericValue(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "header.val", Operator: OperatorGreaterThan, Value: float64(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := envelope("", map[string]any{"val": "not-a-number"}, nil)
	_, err = eval.evaluate(env)
	if err == nil {
		t.Fatal("expected error for non-numeric comparison")
	}
}

func TestCondition_Exists(t *testing.T) {
	tests := []struct {
		name   string
		value  bool
		hdr    map[string]any
		expect bool
	}{
		{"exists true, header present", true, map[string]any{"x": "y"}, true},
		{"exists true, header absent", true, map[string]any{}, false},
		{"exists false, header absent", false, map[string]any{}, true},
		{"exists false, header present", false, map[string]any{"x": "y"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval, err := newConditionEvaluator(Condition{
				Field: "header.x", Operator: OperatorExists, Value: tt.value,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := eval.evaluate(envelope("", tt.hdr, nil))
			if err != nil {
				t.Fatal(err)
			}
			if result != tt.expect {
				t.Fatalf("expected %v, got %v", tt.expect, result)
			}
		})
	}
}

func TestCondition_In(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field:    "header.status",
		Operator: OperatorIn,
		Value:    []any{"active", "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := eval.evaluate(envelope("", map[string]any{"status": "active"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !result {
		t.Fatal("expected true for in-list match")
	}

	result, err = eval.evaluate(envelope("", map[string]any{"status": "deleted"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result {
		t.Fatal("expected false for in-list mismatch")
	}
}

func TestCondition_In_NonSliceValue(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "header.x", Operator: OperatorIn, Value: "not-a-slice",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("", map[string]any{"x": "a"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result {
		t.Fatal("expected false when Value is not a slice")
	}
}

func TestCondition_PayloadExtraction(t *testing.T) {
	payload := map[string]any{
		"order": map[string]any{
			"status": "shipped",
			"total":  42.5,
		},
	}

	eval, err := newConditionEvaluator(Condition{
		Field: "$.order.status", Operator: OperatorEquals, Value: "shipped",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("", nil, payload))
	if err != nil {
		t.Fatal(err)
	}
	if !result {
		t.Fatal("expected payload match")
	}
}

func TestCondition_PayloadExtraction_MissingPath(t *testing.T) {
	payload := map[string]any{"a": 1}

	eval, err := newConditionEvaluator(Condition{
		Field: "$.nonexistent.path", Operator: OperatorEquals, Value: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("", nil, payload))
	if err != nil {
		t.Fatal(err)
	}
	if result {
		t.Fatal("expected false for missing payload path")
	}
}

func TestCondition_NilHeaders(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "header.missing", Operator: OperatorEquals, Value: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result {
		t.Fatal("expected false for nil headers")
	}
}

func TestCondition_EmptyPayload(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "$.some.path", Operator: OperatorEquals, Value: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result {
		t.Fatal("expected false for nil payload")
	}
}

func TestCondition_UnsupportedOperator(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "subject", Operator: "unsupported", Value: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eval.evaluate(envelope("test", nil, nil))
	if err == nil {
		t.Fatal("expected error for unsupported operator")
	}
}

func TestCondition_BareFieldFallsBackToHeaders(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "tenant_id", Operator: OperatorEquals, Value: "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eval.evaluate(envelope("", map[string]any{"tenant_id": "t1"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !result {
		t.Fatal("expected bare field to match header")
	}
}

func TestCondition_StringNumericCoercion(t *testing.T) {
	eval, err := newConditionEvaluator(Condition{
		Field: "header.count", Operator: OperatorGreaterThan, Value: float64(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := envelope("", map[string]any{"count": "10"}, nil)
	result, err := eval.evaluate(env)
	if err != nil {
		t.Fatal(err)
	}
	if !result {
		t.Fatal("expected string '10' > 5 to be true")
	}
}

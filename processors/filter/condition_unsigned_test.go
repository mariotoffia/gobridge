package filter

import (
	"math"
	"testing"
)

// TestCondition_GreaterThan_UnsignedHeaderValues is the regression:
// toFloat64 had no unsigned cases, so a uint/uint8/uint16/uint32/uint64 header
// value in a gt/lt condition returned "cannot convert" -> ErrInvalidPayload
// (Rejected -> DLQ) for a perfectly numeric value. Every unsigned width must now
// coerce, including a uint64 above math.MaxInt64 (handled via direct float
// conversion, which has no int64 representation).
func TestCondition_GreaterThan_UnsignedHeaderValues(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"uint", uint(42)},
		{"uint8", uint8(42)},
		{"uint16", uint16(42)},
		{"uint32", uint32(42)},
		{"uint64", uint64(42)},
		{"uint64 above MaxInt64", uint64(math.MaxInt64) + 100},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eval, err := newConditionEvaluator(Condition{
				Field: "header.count", Operator: OperatorGreaterThan, Value: 5,
			})
			if err != nil {
				t.Fatalf("newConditionEvaluator: %v", err)
			}

			match, err := eval.evaluate(envelope("s", map[string]any{"count": tc.value}, nil))
			if err != nil {
				t.Fatalf("evaluate returned error for numeric %T header value: %v", tc.value, err)
			}
			if !match {
				t.Fatalf("expected %v > 5 to evaluate true", tc.value)
			}
		})
	}
}

// TestToFloat64_UnsignedWidths pins toFloat64's unsigned coverage directly,
// including the uint64-above-MaxInt64 boundary.
func TestToFloat64_UnsignedWidths(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
	}{
		{"uint", uint(7), 7},
		{"uint8", uint8(8), 8},
		{"uint16", uint16(16), 16},
		{"uint32", uint32(32), 32},
		{"uint64", uint64(64), 64},
		{"uint64 max", uint64(math.MaxUint64), float64(math.MaxUint64)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toFloat64(tc.in)
			if err != nil {
				t.Fatalf("toFloat64(%T) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("toFloat64(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

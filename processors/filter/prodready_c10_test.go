package filter

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// ═══════════════════════════════════════════════════════════════════
// C10 (HIGH-1, c10-filter-nonfinite): numeric ordering comparisons
// (gt/lt/gte/lte) must reject non-finite operands (NaN, +Inf, -Inf).
//
// strconv.ParseFloat accepts the literal strings "NaN", "Inf",
// "Infinity" WITHOUT error, and a config value can be math.NaN() /
// math.Inf(). Before the fix such an operand reached numericCompare:
// NaN is unordered so it saw neither < nor > and returned 0 -- a
// gte/lte gate then matched as if EQUAL -- while +/-Inf saturated every
// threshold. That let a security/fairness filter fail open or closed on
// message-controlled input.
//
// Contract now: toFloat64 rejects non-finite. A MESSAGE value surfaces
// shared.ErrInvalidPayload (rejected -> DLQ, deterministic, never a
// silent match); a CONFIG value fails construction with
// ErrComparisonValueNotNumeric.
//
// Mutation killed: drop the math.IsNaN/IsInf guard in toFloat64 and
// numericCompare classifies NaN as equality again -- every assertion
// below FAILS.
// ═══════════════════════════════════════════════════════════════════

// nonFiniteStrings are the message-side literals strconv.ParseFloat
// accepts without error into a non-finite float64.
var nonFiniteStrings = []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "Infinity", "-Infinity"}

// TestToFloat64_RejectsNonFinite pins the coercion boundary directly:
// every non-finite operand, whatever its Go type, must be rejected.
func TestToFloat64_RejectsNonFinite(t *testing.T) {
	cases := []struct {
		name string
		in   any
	}{
		{"float64 NaN", math.NaN()},
		{"float64 +Inf", math.Inf(1)},
		{"float64 -Inf", math.Inf(-1)},
		{"float32 NaN", float32(math.NaN())},
		{"float32 +Inf", float32(math.Inf(1))},
		{"json.Number NaN", json.Number("NaN")},
		{"json.Number Inf", json.Number("Inf")},
	}
	for _, s := range nonFiniteStrings {
		cases = append(cases, struct {
			name string
			in   any
		}{"string " + s, s})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Sanity: for string inputs, confirm the raw parser really does
			// accept them (so the test guards a live bypass, not a strawman).
			if s, ok := tc.in.(string); ok {
				if _, err := strconv.ParseFloat(s, 64); err != nil {
					t.Skipf("strconv rejects %q on this platform; not a bypass vector", s)
				}
			}
			got, err := toFloat64(tc.in)
			if err == nil {
				t.Fatalf("toFloat64(%v) = %v, want non-finite rejection error", tc.in, got)
			}
			if !errors.Is(err, errNonFiniteFloat) {
				t.Fatalf("toFloat64(%v) error = %v, want errNonFiniteFloat", tc.in, err)
			}
		})
	}
}

// TestToFloat64_AcceptsFiniteBoundaries guards against over-rejection:
// the largest finite float64 (and normal values) must still pass.
func TestToFloat64_AcceptsFiniteBoundaries(t *testing.T) {
	cases := []any{math.MaxFloat64, -math.MaxFloat64, float64(0), "1e308", "-1e308", json.Number("1.5")}
	for _, in := range cases {
		if _, err := toFloat64(in); err != nil {
			t.Fatalf("toFloat64(%v) rejected a finite value: %v", in, err)
		}
	}
	// A string overflow like "1e400" already errors via strconv (ErrRange);
	// it must not be silently coerced to +Inf.
	if _, err := toFloat64("1e400"); err == nil {
		t.Fatal("toFloat64(\"1e400\") accepted an overflow to +Inf")
	}
}

// TestCondition_NonFiniteConfigValue_RejectedAtConstruction: a NaN/Inf
// CONFIG comparison value is a deterministic misconfiguration and must
// fail the build with ErrComparisonValueNotNumeric, exactly like any
// other non-numeric comparison value.
func TestCondition_NonFiniteConfigValue_RejectedAtConstruction(t *testing.T) {
	values := []struct {
		name string
		v    any
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"string NaN", "NaN"},
		{"string Inf", "Inf"},
	}
	for _, op := range []Operator{OperatorGreaterThan, OperatorLessThan, OperatorGreaterOrEqual, OperatorLessOrEqual} {
		for _, val := range values {
			t.Run(string(op)+"/"+val.name, func(t *testing.T) {
				_, err := newConditionEvaluator(Condition{Field: "$.x", Operator: op, Value: val.v})
				if !errors.Is(err, ErrComparisonValueNotNumeric) {
					t.Fatalf("expected ErrComparisonValueNotNumeric for non-finite config value, got %v", err)
				}
				if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorPermanent {
					t.Fatalf("expected permanent setup error, got %v", err)
				}
			})
		}
	}
}

// TestCondition_NonFiniteMessageValue_IsRejectedNotEqual is the core
// security regression: a message-controlled "NaN"/"Inf" header on a
// gte/lte gate must be a rejected (DLQ) error, NEVER a silent match. The
// pre-fix bug made `header.score <= 100` MATCH on "NaN" (numericCompare
// returned 0), so a deny/threshold gate failed open or closed on hostile
// input.
func TestCondition_NonFiniteMessageValue_IsRejectedNotEqual(t *testing.T) {
	// gte/lte are the poison operators: cmp==0 satisfies both.
	ops := []Operator{OperatorGreaterOrEqual, OperatorLessOrEqual, OperatorGreaterThan, OperatorLessThan}
	for _, op := range ops {
		for _, val := range nonFiniteStrings {
			t.Run(string(op)+"/"+val, func(t *testing.T) {
				if _, err := strconv.ParseFloat(val, 64); err != nil {
					t.Skipf("strconv rejects %q on this platform", val)
				}
				eval, err := newConditionEvaluator(Condition{Field: "header.score", Operator: op, Value: 100})
				if err != nil {
					t.Fatalf("newConditionEvaluator: %v", err)
				}
				match, err := eval.evaluate(envelope("s", map[string]any{"score": val}, nil))
				if err == nil {
					t.Fatalf("%s on non-finite %q matched=%v with no error (fail-open/closed bypass)", op, val, match)
				}
				if match {
					t.Fatalf("%s on non-finite %q returned match=true alongside error", op, val)
				}
				be, ok := shared.AsBridgeError(err)
				if !ok || be.Code != shared.ErrCodeInvalidPayload || be.Class != shared.ErrorRejected {
					t.Fatalf("expected rejected INVALID_PAYLOAD (DLQ), got %v", err)
				}
				if shared.IsRecoverableError(err) {
					t.Fatal("non-finite message value must not be recoverable (would retry forever)")
				}
			})
		}
	}
}

// TestDropFilter_NonFiniteThreshold_DoesNotSilentlyMatch is the
// policy-level regression: a deny rule `header.score <= 100` on a hostile
// "NaN" must not silently DROP (fail closed) as if the threshold were
// met; it must surface a rejected payload error so the runtime DLQs it.
func TestDropFilter_NonFiniteThreshold_DoesNotSilentlyMatch(t *testing.T) {
	proc, err := NewDropFilter("deny-score-le-100",
		Condition{Field: "header.score", Operator: OperatorLessOrEqual, Value: 100})
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := envelope("orders", map[string]any{"score": "NaN"}, nil)
	err = proc.Process(context.Background(), env, nextOK)

	// Before the fix Process returned ErrMessageFiltered (a silent policy
	// drop driven by NaN==threshold). It must now be a rejected payload.
	if errors.Is(err, shared.ErrMessageFiltered) {
		t.Fatal("deny filter matched on NaN score (numericCompare classified NaN as equality)")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok || be.Code != shared.ErrCodeInvalidPayload {
		t.Fatalf("expected rejected INVALID_PAYLOAD for NaN threshold, got %v", err)
	}
}

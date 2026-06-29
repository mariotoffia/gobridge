package runtime

import (
	"encoding/json"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// ---------------------------------------------------------------------------
// Field Extraction
// ---------------------------------------------------------------------------

func TestConditionEval_SubjectField(t *testing.T) {
	eval, err := newConditionEval(MatchCondition{Field: "subject", Operator: OpEquals, Value: Val("orders.new")})
	if err != nil {
		t.Fatalf("newConditionEval: %v", err)
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders.new"})
	ok, err := eval.evaluate(env, newEvalContext())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !ok {
		t.Fatal("expected match")
	}
}

func TestConditionEval_HeaderDotField(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "header.x-tenant", Operator: OpEquals, Value: Val("acme")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"x-tenant": "acme"}})
	ok, err := eval.evaluate(env, newEvalContext())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !ok {
		t.Fatal("expected match")
	}
}

func TestConditionEval_HeaderDotField_MissingKey(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "header.absent", Operator: OpEquals, Value: Val("x")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"other": "val"}})
	ok, _ := eval.evaluate(env, newEvalContext())
	if ok {
		t.Fatal("expected no match for missing header")
	}
}

func TestConditionEval_HeaderDotField_NilHeaders(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "header.x", Operator: OpEquals, Value: Val("x")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{})
	ok, _ := eval.evaluate(env, newEvalContext())
	if ok {
		t.Fatal("expected no match for nil headers")
	}
}

func TestConditionEval_JSONPath_Nested(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "$.order.status", Operator: OpEquals, Value: Val("confirmed")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"order":{"status":"confirmed"}}`)})
	ok, err := eval.evaluate(env, newEvalContext())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !ok {
		t.Fatal("expected match")
	}
}

func TestConditionEval_JSONPath_Deep(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "$.a.b.c.d", Operator: OpEquals, Value: Val("deep")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"a":{"b":{"c":{"d":"deep"}}}}`)})
	ok, _ := eval.evaluate(env, newEvalContext())
	if !ok {
		t.Fatal("expected match on deep path")
	}
}

func TestConditionEval_JSONPath_MissingPath(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "$.order.missing", Operator: OpEquals, Value: Val("x")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"order":{"status":"ok"}}`)})
	ok, _ := eval.evaluate(env, newEvalContext())
	if ok {
		t.Fatal("expected no match for missing path")
	}
}

func TestConditionEval_JSONPath_EmptyPayload(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "$.x", Operator: OpEquals, Value: Val("y")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{})
	ok, _ := eval.evaluate(env, newEvalContext())
	if ok {
		t.Fatal("expected no match for empty payload")
	}
}

func TestConditionEval_JSONPath_InvalidJSON(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "$.x", Operator: OpEquals, Value: Val("y")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{broken}`)})
	ok, _ := eval.evaluate(env, newEvalContext())
	if ok {
		t.Fatal("expected no match for invalid JSON")
	}
}

func TestConditionEval_BareField_FallsBackToHeader(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "tenant", Operator: OpEquals, Value: Val("acme")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"tenant": "acme"}})
	ok, _ := eval.evaluate(env, newEvalContext())
	if !ok {
		t.Fatal("expected match via header fallback")
	}
}

func TestConditionEval_BareField_NilHeaders(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "tenant", Operator: OpEquals, Value: Val("acme")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{})
	ok, _ := eval.evaluate(env, newEvalContext())
	if ok {
		t.Fatal("expected no match for nil headers")
	}
}

// ---------------------------------------------------------------------------
// Operators (table-driven)
// ---------------------------------------------------------------------------

func TestConditionEval_AllOperators(t *testing.T) {
	tests := []struct {
		name    string
		cond    MatchCondition
		env     *messaging.Envelope
		wantOK  bool
		wantErr bool
	}{
		// eq
		{"eq_match", MatchCondition{"subject", OpEquals, Val("orders")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders"}), true, false},
		{"eq_no_match", MatchCondition{"subject", OpEquals, Val("orders")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "users"}), false, false},

		// ne
		{"ne_match", MatchCondition{"subject", OpNotEquals, Val("orders")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "users"}), true, false},
		{"ne_no_match", MatchCondition{"subject", OpNotEquals, Val("orders")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders"}), false, false},

		// prefix
		{"prefix_match", MatchCondition{"subject", OpPrefix, Val("orders.")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders.new"}), true, false},
		{"prefix_no_match", MatchCondition{"subject", OpPrefix, Val("orders.")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "users.new"}), false, false},
		{"prefix_exact", MatchCondition{"subject", OpPrefix, Val("orders")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders"}), true, false},
		{"prefix_not_contains", MatchCondition{"subject", OpPrefix, Val("rder")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders"}), false, false},

		// contains
		{"contains_match", MatchCondition{"subject", OpContains, Val("admin")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "/api/admin/users"}), true, false},
		{"contains_no_match", MatchCondition{"subject", OpContains, Val("admin")}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "/api/public"}), false, false},

		// regex
		{"regex_match", MatchCondition{"subject", OpRegex, Val(`^order-\d+$`)}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "order-123"}), true, false},
		{"regex_no_match", MatchCondition{"subject", OpRegex, Val(`^order-\d+$`)}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "user-abc"}), false, false},

		// gt / lt / gte / lte
		{"gt_match", MatchCondition{"header.priority", OpGreaterThan, Val(float64(5))}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"priority": float64(10)}}), true, false},
		{"gt_equal", MatchCondition{"header.priority", OpGreaterThan, Val(float64(5))}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"priority": float64(5)}}), false, false},
		{"lt_match", MatchCondition{"header.priority", OpLessThan, Val(float64(5))}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"priority": float64(3)}}), true, false},
		{"gte_equal", MatchCondition{"header.priority", OpGTE, Val(float64(5))}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"priority": float64(5)}}), true, false},
		{"lte_equal", MatchCondition{"header.priority", OpLTE, Val(float64(5))}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"priority": float64(5)}}), true, false},

		// exists
		{"exists_true_present", MatchCondition{"header.x", OpExists, Val(true)}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"x": "v"}}), true, false},
		{"exists_true_absent", MatchCondition{"header.x", OpExists, Val(true)}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{}}), false, false},
		{"exists_false_absent", MatchCondition{"header.x", OpExists, Val(false)}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{}}), true, false},
		{"exists_false_present", MatchCondition{"header.x", OpExists, Val(false)}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"x": "v"}}), false, false},

		// in
		{"in_match", MatchCondition{"subject", OpIn, Val([]any{"prod", "staging"})}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "prod"}), true, false},
		{"in_no_match", MatchCondition{"subject", OpIn, Val([]any{"prod", "staging"})}, messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "dev"}), false, false},

		// numeric coercion
		{"numeric_string", MatchCondition{"header.count", OpGreaterThan, Val(float64(5))}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"count": "10"}}), true, false},
		{"numeric_json_number", MatchCondition{"$.priority", OpGreaterThan, Val(float64(5))}, messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"priority":10}`)}), true, false},
		{"numeric_non_numeric_err", MatchCondition{"header.x", OpGreaterThan, Val(float64(5))}, messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"x": "abc"}}), false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eval, err := newConditionEval(tc.cond)
			if err != nil {
				t.Fatalf("newConditionEval: %v", err)
			}
			ok, err := eval.evaluate(tc.env, newEvalContext())
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("got %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Construction Errors
// ---------------------------------------------------------------------------

func TestConditionEval_InvalidRegex(t *testing.T) {
	_, err := newConditionEval(MatchCondition{Field: "subject", Operator: OpRegex, Value: Val("[invalid(")})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestConditionEval_RegexNonString(t *testing.T) {
	_, err := newConditionEval(MatchCondition{Field: "subject", Operator: OpRegex, Value: Val(42)})
	if err == nil {
		t.Fatal("expected error for non-string regex value")
	}
}

func TestConditionEval_UnknownOperator(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "subject", Operator: "bogus", Value: Val("x")})
	_, err := eval.evaluate(messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "x"}), newEvalContext())
	if err == nil {
		t.Fatal("expected error for unknown operator")
	}
}

func TestConditionEval_RegexPatternTooLong(t *testing.T) {
	longPattern := make([]byte, maxRegexPatternLen+1)
	for i := range longPattern {
		longPattern[i] = 'a'
	}
	_, err := newConditionEval(MatchCondition{Field: "subject", Operator: OpRegex, Value: Val(string(longPattern))})
	if err == nil {
		t.Fatal("expected error for pattern exceeding max length")
	}
}

// ---------------------------------------------------------------------------
// NaN / Inf rejection
// ---------------------------------------------------------------------------

func TestConditionEval_NaN_Rejected(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "header.x", Operator: OpGreaterThan, Value: Val(float64(5))})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"x": "NaN"}})
	_, err := eval.evaluate(env, newEvalContext())
	if err == nil {
		t.Fatal("expected error for NaN")
	}
}

func TestConditionEval_Inf_Rejected(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "header.x", Operator: OpGreaterThan, Value: Val(float64(5))})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"x": "Inf"}})
	_, err := eval.evaluate(env, newEvalContext())
	if err == nil {
		t.Fatal("expected error for Inf")
	}
}

// ---------------------------------------------------------------------------
// Regex input length bound
// ---------------------------------------------------------------------------

func TestConditionEval_RegexInputTooLong_NoMatch(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "subject", Operator: OpRegex, Value: Val(".*")})
	longSubject := make([]byte, maxRegexInputLen+1)
	for i := range longSubject {
		longSubject[i] = 'a'
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: string(longSubject)})
	ok, _ := eval.evaluate(env, newEvalContext())
	if ok {
		t.Fatal("expected no match for input exceeding max regex input length")
	}
}

// ---------------------------------------------------------------------------
// evalContext caching
// ---------------------------------------------------------------------------

func TestEvalContext_ParsesOnce(t *testing.T) {
	ctx := newEvalContext()
	payload := []byte(`{"a":"1","b":"2"}`)

	val1, ok1, _ := ctx.extractPayloadPath(payload, "$.a")
	val2, ok2, _ := ctx.extractPayloadPath(payload, "$.b")

	if !ok1 || val1 != "1" {
		t.Fatalf("first path: got %v/%v", val1, ok1)
	}
	if !ok2 || val2 != "2" {
		t.Fatalf("second path: got %v/%v", val2, ok2)
	}

	// Verify the parse happened once by checking parseDone flag.
	if !ctx.parseDone {
		t.Fatal("expected parseDone to be true")
	}
}

func TestEvalContext_JSONNumber_FromPayload(t *testing.T) {
	ctx := newEvalContext()
	payload := []byte(`{"priority":10}`)
	val, ok, _ := ctx.extractPayloadPath(payload, "$.priority")
	if !ok {
		t.Fatal("expected value")
	}
	// json.Unmarshal maps numbers to float64 by default.
	f, ok := val.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T", val)
	}
	if f != 10 {
		t.Fatalf("expected 10, got %v", f)
	}
}

func TestConditionEval_EmptySubject_ExistsTrue(t *testing.T) {
	eval, _ := newConditionEval(MatchCondition{Field: "subject", Operator: OpExists, Value: Val(true)})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: ""})
	ok, _ := eval.evaluate(env, newEvalContext())
	if !ok {
		t.Fatal("subject field always exists (even if empty)")
	}
}

func init() {
	// Ensure json.Number is available for tests.
	_ = json.Number("0")
}

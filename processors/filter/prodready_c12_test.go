package filter

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// C12 (HIGH, c12-filter-leak): filter numeric conversions must NOT leak
// raw payload/header values into errors, route logs, or DLQ error
// metadata. A gt/lt/gte/lte comparison on a non-numeric field previously
// formatted the raw failing value (`parse float %q`, plus the value
// embedded in strconv's own NumError.Error()), exposing secrets/PII from
// message payloads and headers. The redacted error must report only the
// value's TYPE, LENGTH, the conversion target (float64), and the
// (config-derived) field PATH — never the content.
//
// Mutation killed: revert toFloat64's string case to the raw
// `fmt.Errorf("parse float %q: %w", val, err)` (or wrap the raw
// *strconv.NumError) and the secret leaks back into err.Error() — the
// "secret absent" assertions below FAIL.

// c12Secret is a recognizable, NON-numeric secret so strconv.ParseFloat
// rejects it (ErrSyntax). It must never appear in any error/DLQ metadata.
const c12Secret = "sk_live_51H8qeSUPERSECRETtoken_4f2a9"

// c12PII is a recognizable PII string carried in a header value.
const c12PII = "jane.doe-SSN-078-05-1120"

// assertRedacted verifies err carries a useful-but-content-free numeric
// conversion descriptor: the secret is absent everywhere the runtime
// would surface (the flattened Error() string AND the BridgeError
// Context that becomes DLQ metadata), while the redacted descriptor
// (target type + observed length) and the given debug hint are present.
func assertRedacted(t *testing.T, err error, secret, wantDebugHint string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a conversion error, got nil")
	}

	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Fatalf("SECRET LEAK: error message contains the raw value %q: %s", secret, msg)
	}

	// DLQ error metadata is built from BridgeError.Context; the raw value
	// must not hide there either.
	if be, ok := shared.AsBridgeError(err); ok {
		for k, v := range be.Context {
			if s, isStr := v.(string); isStr && strings.Contains(s, secret) {
				t.Fatalf("SECRET LEAK: DLQ metadata %q contains the raw value %q", k, secret)
			}
		}
	}

	// Useful for debugging: conversion target + observed length must be
	// present so an operator can diagnose without the content.
	if !strings.Contains(msg, "to float64") {
		t.Fatalf("redacted error missing conversion target %q: %s", "to float64", msg)
	}
	if !strings.Contains(msg, "len ") {
		t.Fatalf("redacted error missing observed length descriptor: %s", msg)
	}
	if wantDebugHint != "" && !strings.Contains(msg, wantDebugHint) {
		t.Fatalf("redacted error missing debug hint %q: %s", wantDebugHint, msg)
	}
}

// TestFilter_NumericConvert_DoesNotLeakSecret drives a non-numeric
// payload/header value through the ordering operators end-to-end and via
// the evaluator, asserting the resulting (DLQ-bound) error redacts the
// value while staying diagnosable (field PATH + target type + length).
func TestFilter_NumericConvert_DoesNotLeakSecret(t *testing.T) {
	tests := []struct {
		name      string
		cond      Condition
		env       *messaging.Envelope
		secret    string
		debugHint string // config-derived field PATH — safe to surface
	}{
		{
			name:      "payload value gt",
			cond:      Condition{Field: "$.token", Operator: OperatorGreaterThan, Value: 5},
			env:       envelope("orders", nil, json.RawMessage(`{"token":"`+c12Secret+`"}`)),
			secret:    c12Secret,
			debugHint: "$.token",
		},
		{
			name:      "header value lte",
			cond:      Condition{Field: "header.ssn", Operator: OperatorLessOrEqual, Value: 100},
			env:       envelope("orders", map[string]any{"ssn": c12PII}, nil),
			secret:    c12PII,
			debugHint: "header.ssn",
		},
		{
			name:      "bare header value gte",
			cond:      Condition{Field: "apikey", Operator: OperatorGreaterOrEqual, Value: 0},
			env:       envelope("orders", map[string]any{"apikey": c12Secret}, nil),
			secret:    c12Secret,
			debugHint: "apikey",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval, err := newConditionEvaluator(tt.cond)
			if err != nil {
				t.Fatalf("newConditionEvaluator: %v", err)
			}
			_, err = eval.evaluate(tt.env)

			assertRedacted(t, err, tt.secret, tt.debugHint)

			// Classification is unchanged: a non-numeric message field is
			// a rejected payload (DLQ), and the low-level reason is
			// preserved for debugging without carrying the value.
			if !errors.Is(err, shared.ErrInvalidPayload) {
				t.Fatalf("expected ErrInvalidPayload, got %v", err)
			}
			if !errors.Is(err, strconv.ErrSyntax) {
				t.Fatalf("expected wrapped strconv.ErrSyntax reason, got %v", err)
			}
		})
	}
}

// TestFilter_NumericConvert_DropFilterErrorRedacted is the end-to-end
// policy path: a drop filter whose gt condition trips on a secret-bearing
// field returns a DLQ error that does NOT echo the secret.
func TestFilter_NumericConvert_DropFilterErrorRedacted(t *testing.T) {
	proc, err := NewDropFilter("threshold",
		Condition{Field: "$.token", Operator: OperatorGreaterThan, Value: 5})
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := envelope("orders", nil, json.RawMessage(`{"token":"`+c12Secret+`"}`))
	got := proc.Process(context.Background(), env, nextOK)

	assertRedacted(t, got, c12Secret, "$.token")
}

// TestToFloat64_RedactsRawValue pins the redaction at the conversion
// boundary itself for both string and json.Number inputs (the two verbs
// that carried the raw value). len(secret) must be reported; the content
// must not.
func TestToFloat64_RedactsRawValue(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		_, err := toFloat64(c12Secret)
		assertRedacted(t, err, c12Secret, "string")
		if !strings.Contains(err.Error(), strconv.Itoa(len(c12Secret))) {
			t.Fatalf("expected observed length %d in %q", len(c12Secret), err.Error())
		}
	})

	t.Run("json.Number", func(t *testing.T) {
		_, err := toFloat64(json.Number(c12Secret))
		assertRedacted(t, err, c12Secret, "json.Number")
		if !strings.Contains(err.Error(), strconv.Itoa(len(c12Secret))) {
			t.Fatalf("expected observed length %d in %q", len(c12Secret), err.Error())
		}
	})
}

// TestComparisonValue_ConstructionError_DoesNotLeak is defense in depth:
// even the setup-time ErrComparisonValueNotNumeric (config-derived value)
// must not echo the raw value, since a comparison constant may itself be
// a secret. Classification and errors.Is identity are preserved.
func TestComparisonValue_ConstructionError_DoesNotLeak(t *testing.T) {
	_, err := newConditionEvaluator(Condition{
		Field: "$.x", Operator: OperatorGreaterThan, Value: c12Secret,
	})
	if !errors.Is(err, ErrComparisonValueNotNumeric) {
		t.Fatalf("expected ErrComparisonValueNotNumeric, got %v", err)
	}
	if strings.Contains(err.Error(), c12Secret) {
		t.Fatalf("SECRET LEAK: construction error contains the raw value %q: %s", c12Secret, err.Error())
	}
}

// c12MalformedSecret is a secret-bearing payload that is NOT valid JSON.
// Its leading byte '@' is exactly the byte encoding/json's *json.SyntaxError
// quotes ("invalid character '@' ...") — so the un-redacted path leaks a
// payload byte into route-runner logs and DLQ metadata.
const c12MalformedSecret = "@Zk_live_MALFORMED_SECRET_9f3c"

// TestFilter_MalformedJSONPayload_DoesNotLeakSecret is the SAME-CLASS
// payload-leak sibling of the numeric fix: a "$." path condition over a
// malformed-JSON payload must not surface any payload byte. encoding/json's
// *json.SyntaxError.Error() quotes the first offending byte; the redacted
// error carries only the content-free byte OFFSET (plus the config-derived
// field PATH) while preserving the fail-closed ErrInvalidPayload class.
//
// Mutation killed: revert redactJSONParseErr to
// `fmt.Errorf("filter: %q path on unparseable JSON payload: %w", path, err)`
// and the quoted payload byte ('@') / "invalid character" classifier leak
// back into err.Error() — the assertions below FAIL.
func TestFilter_MalformedJSONPayload_DoesNotLeakSecret(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders"})
	env.SetPayload([]byte(c12MalformedSecret)) // raw, malformed on purpose

	eval, err := newConditionEvaluator(Condition{Field: "$.x", Operator: OperatorEquals, Value: 1})
	if err != nil {
		t.Fatalf("newConditionEvaluator: %v", err)
	}

	_, err = eval.evaluate(env)
	if err == nil {
		t.Fatal("expected a rejected error for malformed JSON payload")
	}
	msg := err.Error()

	if strings.Contains(msg, c12MalformedSecret) {
		t.Fatalf("SECRET LEAK: error contains the raw payload %q: %s", c12MalformedSecret, msg)
	}
	// json.SyntaxError quotes the first offending byte ('@', a payload
	// byte); neither it nor the quoting classifier may survive redaction.
	if strings.Contains(msg, "invalid character") || strings.Contains(msg, "@") {
		t.Fatalf("SECRET LEAK: error echoes a payload byte: %s", msg)
	}
	// Redacted-but-diagnosable: content-free offset + config-derived path.
	if !strings.Contains(msg, "malformed JSON") || !strings.Contains(msg, "offset") {
		t.Fatalf("expected redacted offset classifier, got: %s", msg)
	}
	if !strings.Contains(msg, "$.x") {
		t.Fatalf("expected field path for debugging, got: %s", msg)
	}
	// Fail-closed classification preserved (rejected -> DLQ, not silent no-match).
	if !errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// valueBearingErr is a leaf error (Unwrap() == nil) whose Error() embeds a
// value. It models a future caller handing the reusable redactor a bare or
// foreign error; the redactor must never surface its Error().
type valueBearingErr struct{ msg string }

func (e valueBearingErr) Error() string { return e.msg }

// TestRedactNumericConvErr_NeverEchoesValue pins the reusable redactor's
// content-free contract independent of caller input: a wrapped strconv
// sentinel is surfaced (and errors.Is preserved) with zero content, and a
// value-bearing error whose Unwrap() is nil is reduced to a generic reason
// rather than echoed.
//
// Mutation killed: revert the redactor's nil-Unwrap branch to fall back to
// `reason = cause` + `%w`, and the "unwrappable value-bearing" case leaks
// the secret into err.Error() — the "secret absent" assertion FAILs.
func TestRedactNumericConvErr_NeverEchoesValue(t *testing.T) {
	tests := []struct {
		name       string
		cause      error
		wantReason string // MUST be present
		wantIs     error  // sentinel errors.Is must still match (nil = none)
	}{
		{
			name:       "wrapped strconv sentinel preserved",
			cause:      &strconv.NumError{Func: "ParseFloat", Num: c12Secret, Err: strconv.ErrSyntax},
			wantReason: "invalid syntax",
			wantIs:     strconv.ErrSyntax,
		},
		{
			name:       "unwrappable value-bearing error is not echoed",
			cause:      valueBearingErr{msg: c12Secret},
			wantReason: "conversion error",
			wantIs:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// value "abc" is a placeholder; %T only ever reveals the type.
			err := redactNumericConvErr("abc", len(c12Secret), tt.cause)
			got := err.Error()

			if strings.Contains(got, c12Secret) {
				t.Fatalf("SECRET LEAK: redactor echoed the raw value: %s", got)
			}
			if !strings.Contains(got, tt.wantReason) {
				t.Fatalf("expected reason %q, got %q", tt.wantReason, got)
			}
			if !strings.Contains(got, "to float64") || !strings.Contains(got, "len ") {
				t.Fatalf("expected redacted descriptor (target + length), got %q", got)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Fatalf("expected errors.Is(%v) preserved, got %q", tt.wantIs, got)
			}
		})
	}
}

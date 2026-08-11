package transform

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func c12noop(_ context.Context, _ *messaging.Envelope) error { return nil }

// c12-transform-empty (bypass closure): empty bytes, whitespace-only input,
// and a literal {} are three representations of "zero fields". A Required
// mapping with NO default is unsatisfiable on all three, so all three MUST
// reject with the IDENTICAL rejected error — no representation may slip
// through. This is the security proof.
//
// Mutation killed: reinstating the pre-fix empty-bytes passthrough
// short-circuit (`if len(payload)==0 { return next(ctx,env) }`, dropping the
// nil→{} normalization) makes the empty-bytes case call next with no error →
// this test FAILs.
func TestJSONTransform_ZeroFieldInputs_RequiredRejectUniformly(t *testing.T) {
	inputs := []struct {
		name    string
		payload []byte
	}{
		{"nil bytes", nil},
		{"empty bytes", []byte{}},
		{"whitespace only", []byte("   \t\n ")},
		{"empty object", []byte("{}")},
	}

	var (
		firstErr string
		firstSet bool
	)
	for _, in := range inputs {
		in := in
		t.Run(in.name, func(t *testing.T) {
			proc, err := New(Config{Mappings: []FieldMapping{RequiredMapping("$.id", "id")}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: in.payload})

			called := false
			err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
				called = true
				return nil
			})
			if called {
				t.Fatalf("next must NOT be called: %q cannot satisfy a required mapping", in.name)
			}
			if !errors.Is(err, shared.ErrInvalidPayload) {
				t.Fatalf("expected rejected ErrInvalidPayload, got %v", err)
			}
			if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorRejected {
				t.Fatalf("expected rejected BridgeError (DLQ-not-retry), got %v", err)
			}
			if shared.IsRecoverableError(err) {
				t.Fatal("required-field rejection must not be runtime-retryable")
			}
			// All zero-field representations must yield the SAME error text —
			// convergence, not three different behaviours.
			if !firstSet {
				firstErr, firstSet = err.Error(), true
			} else if err.Error() != firstErr {
				t.Fatalf("divergent rejection: %q gave %q, want %q", in.name, err.Error(), firstErr)
			}
		})
	}
}

// c12-transform-empty (default parity): a Required mapping WITH a default is
// SATISFIED from that default on empty bytes exactly as it is on {} — the case
// the coarse `hasRequired` gate wrongly DLQ'd. Both paths must yield identical
// output and no error.
func TestJSONTransform_ZeroFieldInputs_RequiredWithDefault_MatchesEmptyObject(t *testing.T) {
	newProc := func(t *testing.T) *Processor {
		t.Helper()
		proc, err := New(Config{Mappings: []FieldMapping{{
			Source:       "$.id",
			Target:       "id",
			Required:     true,
			DefaultValue: "D",
		}}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return proc
	}

	run := func(t *testing.T, payload []byte) []byte {
		t.Helper()
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: payload})
		if err := newProc(t).Process(context.Background(), env, c12noop); err != nil {
			t.Fatalf("expected success from default, got %v", err)
		}
		return env.Payload()
	}

	want := run(t, []byte("{}"))
	if string(want) != `{"id":"D"}` {
		t.Fatalf("baseline {} output = %s, want {\"id\":\"D\"}", want)
	}
	for _, empty := range [][]byte{nil, {}, []byte("   ")} {
		if got := run(t, empty); string(got) != string(want) {
			t.Fatalf("empty %q output = %s, want parity with {} → %s", empty, got, want)
		}
	}
}

// c12-transform-empty (passthrough preserved): a non-required, non-FailOnError
// config must STILL pass an empty/whitespace body straight through — next is
// called, no error, bytes unchanged.
func TestJSONTransform_EmptyPayload_BestEffortStillPasses(t *testing.T) {
	for _, payload := range [][]byte{nil, {}, []byte("  ")} {
		proc, err := New(Config{Mappings: []FieldMapping{SimpleMapping("$.name", "out")}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: payload})

		called := false
		err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
			called = true
			return nil
		})
		if err != nil {
			t.Fatalf("expected best-effort pass-through, got %v", err)
		}
		if !called {
			t.Fatal("next must be called on the legitimate empty-payload pass-through")
		}
		if !bytes.Equal(env.Payload(), payload) {
			t.Fatalf("empty payload must pass through unchanged, got %q", env.Payload())
		}
	}
}

// c12-transform-leak: a conversion failure on a payload value that carries a
// recognizable secret must NOT echo the raw value (or a strconv error that
// embeds it) into the returned error — only a redacted TYPE/LENGTH descriptor,
// the field PATH, and the target type may appear. Reverting to the raw `%q`
// (or the `%w` strconv chain) makes this FAIL.
func TestJSONTransform_ConversionError_RedactsPayloadValue(t *testing.T) {
	const secret = "4111111111111111-CVV-999-PAN"

	cases := []struct {
		name      string
		transform TransformType
		source    string
		target    string
		// payload is a single-field object whose value is the secret string.
		payload string
	}{
		{"int", TransformInt, "$.card", "card_num", `{"card":"` + secret + `"}`},
		{"float", TransformFloat, "$.card", "card_num", `{"card":"` + secret + `"}`},
		{"bool", TransformBool, "$.card", "card_flag", `{"card":"` + secret + `"}`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			proc, err := New(Config{
				FailOnError: true,
				Mappings:    []FieldMapping{TransformedMapping(tc.source, tc.target, tc.transform)},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(tc.payload)})

			err = proc.Process(context.Background(), env, c12noop)
			if err == nil {
				t.Fatalf("expected conversion failure for %q -> %s", secret, tc.transform)
			}

			// Walk the whole wrapped error chain, not just the top message.
			msg := err.Error()
			if strings.Contains(msg, secret) {
				t.Fatalf("SECRET LEAKED: error message contains the raw payload value: %q", msg)
			}
			// strconv's own error text embeds the raw value; it must be absent
			// too (i.e. the %w chain to strconv must have been dropped).
			if strings.Contains(msg, "parsing \"") {
				t.Fatalf("SECRET LEAKED via strconv error: %q", msg)
			}
			// A redacted, content-free descriptor must be present instead.
			if !strings.Contains(msg, "type=string") || !strings.Contains(msg, "len=") {
				t.Fatalf("expected redacted descriptor (type=string len=N), got %q", msg)
			}
			// The field PATH must still be reported for debuggability.
			if !strings.Contains(msg, tc.source) {
				t.Fatalf("expected source path %q in error, got %q", tc.source, msg)
			}
		})
	}
}

// c12-transform-leak (unit): the low-level converters must never surface the
// raw failed value nor the strconv error that embeds it.
func TestRedactValue_ContentFreeDescriptor(t *testing.T) {
	const secret = "hunter2-topsecret"

	if _, err := toInt(secret); err == nil {
		t.Fatal("expected toInt error")
	} else if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "type=string len=") {
		t.Fatalf("toInt leak/redaction: %v", err.Error())
	}
	if _, err := toFloat(secret); err == nil {
		t.Fatal("expected toFloat error")
	} else if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "type=string len=") {
		t.Fatalf("toFloat leak/redaction: %v", err.Error())
	}
	if _, err := toBool(secret); err == nil {
		t.Fatal("expected toBool error")
	} else if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "type=string len=") {
		t.Fatalf("toBool leak/redaction: %v", err.Error())
	}

	// redactValue reports LENGTH for length-bearing kinds, TYPE only otherwise.
	if got := redactValue(secret); got != "type=string len=17" {
		t.Fatalf("redactValue(string): got %q", got)
	}
	if got := redactValue([]any{1, 2, 3}); got != "type=array len=3" {
		t.Fatalf("redactValue(array): got %q", got)
	}
	if got := redactValue(map[string]any{"a": 1}); got != "type=object len=1" {
		t.Fatalf("redactValue(object): got %q", got)
	}
	if got := redactValue(3.14); got != "type=float64" {
		t.Fatalf("redactValue(float64): got %q", got)
	}
}

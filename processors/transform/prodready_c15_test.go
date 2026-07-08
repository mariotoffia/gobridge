package transform

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func c15noop(_ context.Context, _ *messaging.Envelope) error { return nil }

// F7: toInt must reject non-finite and out-of-range floats rather than emitting
// implementation-defined garbage (int64(1e300) is typically a silent MinInt64).
func TestToInt_FloatOverflowAndNonFinite(t *testing.T) {
	bad := []struct {
		name string
		in   any
	}{
		{"1e300", float64(1e300)},
		{"-1e300", float64(-1e300)},
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"float32-overflow", float32(math.MaxFloat32)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := toInt(tc.in); err == nil {
				t.Fatalf("toInt(%v) = %d, want error", tc.in, got)
			}
		})
	}

	ok := []struct {
		in   any
		want int64
	}{
		{float64(42), 42},
		{float64(-42.9), -42}, // truncates toward zero
		{float32(7), 7},
	}
	for _, tc := range ok {
		got, err := toInt(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("toInt(%v) = %d, %v; want %d, nil", tc.in, got, err, tc.want)
		}
	}
}

// F7 (integration): a JSON float too large for int64 rejects under
// FailOnError=true and is SKIPPED (not written as MinInt64) under best-effort.
func TestProcess_TransformInt_FloatOverflow(t *testing.T) {
	input := []byte(`{"big": 1e300}`)

	t.Run("FailOnError=true rejects", func(t *testing.T) {
		p, err := New(Config{
			FailOnError: true,
			Mappings:    []FieldMapping{TransformedMapping("$.big", "n", TransformInt)},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), input...)})
		err = p.Process(context.Background(), env, c15noop)
		if err == nil {
			t.Fatal("expected error for 1e300 -> int with FailOnError=true")
		}
		if !errors.Is(err, shared.ErrInvalidPayload) {
			t.Fatalf("expected ErrInvalidPayload, got %v", err)
		}
	})

	t.Run("FailOnError=false skips without MinInt64 garbage", func(t *testing.T) {
		p, err := New(Config{
			Mappings: []FieldMapping{TransformedMapping("$.big", "n", TransformInt)},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), input...)})
		if err := p.Process(context.Background(), env, c15noop); err != nil {
			t.Fatalf("expected skip, got %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(env.Payload(), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if v, present := out["n"]; present {
			t.Fatalf("expected mapping skipped (no 'n'), got n=%v", v)
		}
	})
}

// F8.1: a payload (non-header) mapping with an empty Target is rejected at New.
func TestNew_EmptyPayloadTarget(t *testing.T) {
	_, err := New(Config{Mappings: []FieldMapping{{Source: "$.x", Target: ""}}})
	if err == nil {
		t.Fatal("expected error for empty payload target")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok || be.Message != ErrPayloadTargetEmpty.Message {
		t.Fatalf("expected ErrPayloadTargetEmpty, got %v", err)
	}
}

// F8.2: a target path that crosses an existing scalar must NOT silently replace
// the scalar with a map. FailOnError=true rejects; best-effort skips and leaves
// the scalar intact.
func TestProcess_SetNested_CrossesScalar(t *testing.T) {
	input := []byte(`{"user":"bob"}`)

	t.Run("FailOnError=true rejects", func(t *testing.T) {
		p, err := New(Config{
			FailOnError: true,
			Mappings:    []FieldMapping{SimpleMapping("$.user", "user.name")},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), input...)})
		err = p.Process(context.Background(), env, c15noop)
		if err == nil {
			t.Fatal("expected error crossing a scalar with FailOnError=true")
		}
		if !errors.Is(err, shared.ErrInvalidPayload) {
			t.Fatalf("expected ErrInvalidPayload, got %v", err)
		}
	})

	t.Run("FailOnError=false preserves the scalar", func(t *testing.T) {
		p, err := New(Config{
			Mappings: []FieldMapping{SimpleMapping("$.user", "user.name")},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), input...)})
		if err := p.Process(context.Background(), env, c15noop); err != nil {
			t.Fatalf("expected skip, got %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(env.Payload(), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out["user"] != "bob" {
			t.Fatalf("scalar clobbered: expected user=\"bob\", got %v", out["user"])
		}
	})
}

// F8: a normal nested target still writes through and creates the intermediate
// object.
func TestProcess_SetNested_NormalNestedWrites(t *testing.T) {
	input := []byte(`{"name":"bob"}`)
	p, err := New(Config{Mappings: []FieldMapping{SimpleMapping("$.name", "user.name")}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: append([]byte(nil), input...)})
	if err := p.Process(context.Background(), env, c15noop); err != nil {
		t.Fatalf("Process: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(env.Payload(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	user, ok := out["user"].(map[string]any)
	if !ok || user["name"] != "bob" {
		t.Fatalf("expected user.name=\"bob\", got %v", out)
	}
}

// F9: a DefaultValue incompatible with its own Transform fails fast at New,
// instead of silently vanishing per fallback message.
func TestNew_DefaultValueIncompatibleWithTransform(t *testing.T) {
	_, err := New(Config{Mappings: []FieldMapping{{
		Source:       "$.x",
		Target:       "out",
		Transform:    TransformInt,
		DefaultValue: "abc",
	}}})
	if err == nil {
		t.Fatal("expected error for DefaultValue \"abc\" + int transform")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok || be.Message != ErrDefaultValueTransform.Message {
		t.Fatalf("expected ErrDefaultValueTransform, got %v", err)
	}
}

func TestNew_DefaultValueCompatibleWithTransform(t *testing.T) {
	_, err := New(Config{Mappings: []FieldMapping{{
		Source:       "$.x",
		Target:       "out",
		Transform:    TransformInt,
		DefaultValue: 5,
	}}})
	if err != nil {
		t.Fatalf("expected success for DefaultValue 5 + int transform, got %v", err)
	}
}

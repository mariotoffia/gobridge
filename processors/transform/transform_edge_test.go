package transform

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

func TestJSONTransform_FailOnError_StopsOnMappingFailure(t *testing.T) {
	input := map[string]any{"other": "data"}
	inputBytes, _ := json.Marshal(input)
	noop := func(_ context.Context, _ *messaging.Envelope) error { return nil }

	t.Run("FailOnError=true with required missing field returns error", func(t *testing.T) {
		proc, err := New(Config{
			FailOnError: true,
			Mappings: []FieldMapping{
				RequiredMapping("$.nonexistent", "out"),
			},
		})
		if err != nil {
			t.Fatalf("failed to create processor: %v", err)
		}

		env := &messaging.Envelope{Payload: append([]byte(nil), inputBytes...)}
		if err := proc.Process(context.Background(), env, noop); err == nil {
			t.Fatal("expected error for required missing field with FailOnError=true")
		}
	})

	t.Run("FailOnError=false with optional missing field succeeds", func(t *testing.T) {
		proc, err := New(Config{
			FailOnError: false,
			Mappings: []FieldMapping{
				SimpleMapping("$.nonexistent", "out"),
			},
		})
		if err != nil {
			t.Fatalf("failed to create processor: %v", err)
		}

		env := &messaging.Envelope{Payload: append([]byte(nil), inputBytes...)}
		if err := proc.Process(context.Background(), env, noop); err != nil {
			t.Fatalf("expected success when FailOnError=false, got: %v", err)
		}
	})

	t.Run("FailOnError=true with failing transform returns error", func(t *testing.T) {
		proc, err := New(Config{
			FailOnError: true,
			Mappings: []FieldMapping{
				TransformedMapping("$.other", "num", TransformInt),
			},
		})
		if err != nil {
			t.Fatalf("failed to create processor: %v", err)
		}

		env := &messaging.Envelope{Payload: append([]byte(nil), inputBytes...)}
		if err := proc.Process(context.Background(), env, noop); err == nil {
			t.Fatal("expected error converting non-numeric string to int with FailOnError=true")
		}
	})

	t.Run("FailOnError=false with failing transform skips mapping", func(t *testing.T) {
		proc, err := New(Config{
			FailOnError: false,
			Mappings: []FieldMapping{
				TransformedMapping("$.other", "num", TransformInt),
			},
		})
		if err != nil {
			t.Fatalf("failed to create processor: %v", err)
		}

		env := &messaging.Envelope{Payload: append([]byte(nil), inputBytes...)}
		if err := proc.Process(context.Background(), env, noop); err != nil {
			t.Fatalf("expected mapping to be skipped when FailOnError=false, got: %v", err)
		}
	})
}

func TestJSONTransform_TransformFloat(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{
			TransformedMapping("$.price", "price_num", TransformFloat),
		},
		DropUnmapped: true,
	})
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	input := map[string]any{"price": "42.5"}
	inputBytes, _ := json.Marshal(input)
	env := &messaging.Envelope{Payload: inputBytes}

	err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	v, ok := output["price_num"]
	if !ok {
		t.Fatal("expected price_num in output")
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("expected price_num to be float64, got %T", v)
	}
	if f != 42.5 {
		t.Errorf("expected price_num=42.5, got %v", f)
	}
}

func TestApplyTransform_UnsupportedType(t *testing.T) {
	_, err := applyTransform("hello", TransformType("xml"))
	if err == nil {
		t.Fatal("expected error for unsupported transform type")
	}
}

func TestProcessor_Name_DefaultAndConfigured(t *testing.T) {
	t.Run("configured name", func(t *testing.T) {
		proc, err := New(Config{Name: "my-xform"})
		if err != nil {
			t.Fatalf("failed to create processor: %v", err)
		}
		if got := proc.Name(); got != "my-xform" {
			t.Errorf("expected name my-xform, got %s", got)
		}
	})

	t.Run("default name", func(t *testing.T) {
		proc, err := New(Config{})
		if err != nil {
			t.Fatalf("failed to create processor: %v", err)
		}
		if got := proc.Name(); got != "transform" {
			t.Errorf("expected name transform, got %s", got)
		}
	})
}

func TestJSONTransform_TransformNone(t *testing.T) {
	proc, err := New(Config{
		Mappings: []FieldMapping{
			{Source: "$.status", Target: "status_copy", Transform: TransformNone},
		},
		DropUnmapped: true,
	})
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	input := map[string]any{"status": "active"}
	inputBytes, _ := json.Marshal(input)
	env := &messaging.Envelope{Payload: inputBytes}

	err = proc.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if output["status_copy"] != "active" {
		t.Errorf("expected status_copy=active, got %v", output["status_copy"])
	}
}

package transform

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Validates basic field renaming for simple JSON path mappings.
func TestJSONTransform_SimpleMapping(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			SimpleMapping("$.name", "userName"),
			SimpleMapping("$.age", "userAge"),
		},
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	input := map[string]any{
		"name": "alice",
		"age":  30,
	}
	inputBytes, _ := json.Marshal(input)

	env := &messaging.Envelope{Payload: inputBytes}

	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	// Original fields preserved
	if output["name"] != "alice" {
		t.Errorf("expected name=alice, got %v", output["name"])
	}

	// New fields added
	if output["userName"] != "alice" {
		t.Errorf("expected userName=alice, got %v", output["userName"])
	}
	if output["userAge"] != float64(30) {
		t.Errorf("expected userAge=30, got %v", output["userAge"])
	}
}

// Validates nested field extraction with DropUnmapped.
func TestJSONTransform_NestedExtraction(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			SimpleMapping("$.user.profile.name", "name"),
			SimpleMapping("$.user.profile.email", "email"),
		},
		DropUnmapped: true,
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	input := map[string]any{
		"user": map[string]any{
			"profile": map[string]any{
				"name":  "bob",
				"email": "bob@example.com",
			},
		},
	}
	inputBytes, _ := json.Marshal(input)

	env := &messaging.Envelope{Payload: inputBytes}

	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if output["name"] != "bob" {
		t.Errorf("expected name=bob, got %v", output["name"])
	}
	if output["email"] != "bob@example.com" {
		t.Errorf("expected email=bob@example.com, got %v", output["email"])
	}

	// Original fields should NOT be present (DropUnmapped=true)
	if _, ok := output["user"]; ok {
		t.Error("expected user field to be dropped")
	}
}

// Validates building nested target objects from flat source fields.
func TestJSONTransform_NestedTarget(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			SimpleMapping("$.firstName", "user.name.first"),
			SimpleMapping("$.lastName", "user.name.last"),
		},
		DropUnmapped: true,
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	input := map[string]any{
		"firstName": "John",
		"lastName":  "Doe",
	}
	inputBytes, _ := json.Marshal(input)

	env := &messaging.Envelope{Payload: inputBytes}

	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	user, ok := output["user"].(map[string]any)
	if !ok {
		t.Fatal("expected user object")
	}
	name, ok := user["name"].(map[string]any)
	if !ok {
		t.Fatal("expected user.name object")
	}
	if name["first"] != "John" {
		t.Errorf("expected first=John, got %v", name["first"])
	}
	if name["last"] != "Doe" {
		t.Errorf("expected last=Doe, got %v", name["last"])
	}
}

// Validates string, int, and bool transform mappings.
func TestJSONTransform_TypeTransformation(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			TransformedMapping("$.count", "countStr", TransformString),
			TransformedMapping("$.value", "valueInt", TransformInt),
			TransformedMapping("$.flag", "flagBool", TransformBool),
		},
		DropUnmapped: true,
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	input := map[string]any{
		"count": 42,
		"value": "123",
		"flag":  "true",
	}
	inputBytes, _ := json.Marshal(input)

	env := &messaging.Envelope{Payload: inputBytes}

	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if output["countStr"] != "42" {
		t.Errorf("expected countStr=42, got %v", output["countStr"])
	}
	if output["valueInt"] != float64(123) { // JSON numbers are float64
		t.Errorf("expected valueInt=123, got %v", output["valueInt"])
	}
	if output["flagBool"] != true {
		t.Errorf("expected flagBool=true, got %v", output["flagBool"])
	}
}

// Validates default values when source paths are missing.
func TestJSONTransform_DefaultValue(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			{
				Source:       "$.missing",
				Target:       "value",
				DefaultValue: "default-value",
			},
			{
				Source:       "$.alsoMissing",
				Target:       "count",
				DefaultValue: 0,
			},
		},
		DropUnmapped: true,
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	input := map[string]any{
		"other": "data",
	}
	inputBytes, _ := json.Marshal(input)

	env := &messaging.Envelope{Payload: inputBytes}

	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if output["value"] != "default-value" {
		t.Errorf("expected value=default-value, got %v", output["value"])
	}
	if output["count"] != float64(0) {
		t.Errorf("expected count=0, got %v", output["count"])
	}
}

// Validates error when a required mapping source is absent.
func TestJSONTransform_RequiredField(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			RequiredMapping("$.required", "output"),
		},
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	input := map[string]any{
		"other": "data",
	}
	inputBytes, _ := json.Marshal(input)

	env := &messaging.Envelope{Payload: inputBytes}

	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		return nil
	})

	if err == nil {
		t.Error("expected error for missing required field")
	}
}

// Validates base64 encode and decode transforms.
func TestJSONTransform_Base64Encoding(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			TransformedMapping("$.plain", "encoded", TransformBase64Encode),
			TransformedMapping("$.encoded", "decoded", TransformBase64Decode),
		},
		DropUnmapped: true,
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	input := map[string]any{
		"plain":   "hello world",
		"encoded": "aGVsbG8gd29ybGQ=",
	}
	inputBytes, _ := json.Marshal(input)

	env := &messaging.Envelope{Payload: inputBytes}

	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if output["encoded"] != "aGVsbG8gd29ybGQ=" {
		t.Errorf("expected encoded=aGVsbG8gd29ybGQ=, got %v", output["encoded"])
	}
	if output["decoded"] != "hello world" {
		t.Errorf("expected decoded=hello world, got %v", output["decoded"])
	}
}

// Validates array index access in JSONPath sources.
func TestJSONTransform_ArrayAccess(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			SimpleMapping("$.items[0].name", "firstName"),
			SimpleMapping("$.items[1].name", "secondName"),
		},
		DropUnmapped: true,
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	input := map[string]any{
		"items": []any{
			map[string]any{"name": "first"},
			map[string]any{"name": "second"},
		},
	}
	inputBytes, _ := json.Marshal(input)

	env := &messaging.Envelope{Payload: inputBytes}

	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(env.Payload, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if output["firstName"] != "first" {
		t.Errorf("expected firstName=first, got %v", output["firstName"])
	}
	if output["secondName"] != "second" {
		t.Errorf("expected secondName=second, got %v", output["secondName"])
	}
}

// Validates Process with nil payload still invokes next.
func TestJSONTransform_EmptyPayload(t *testing.T) {
	transform, err := New(Config{
		Mappings: []FieldMapping{
			SimpleMapping("$.name", "output"),
		},
	})
	if err != nil {
		t.Fatalf("failed to create transform: %v", err)
	}

	env := &messaging.Envelope{Payload: nil}

	nextCalled := false
	err = transform.Process(context.Background(), env, func(ctx context.Context, e *messaging.Envelope) error {
		nextCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if !nextCalled {
		t.Error("expected next to be called for empty payload")
	}
}

// Validates New rejects invalid JSONPath in mappings.
func TestJSONTransform_InvalidJSONPath(t *testing.T) {
	_, err := New(Config{
		Mappings: []FieldMapping{
			SimpleMapping("$[invalid", "output"),
		},
	})

	if err == nil {
		t.Error("expected error for invalid JSONPath")
	}
}

package filter

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// Filter Middleware Unit Tests
//
// Tests for the filter middleware covering:
// - Condition evaluation (equals, contains, regex, numeric comparisons)
// - Filter actions (pass, drop, route)
// - Field extraction (topic, metadata, JSONPath)
// - Inversion logic
// ═══════════════════════════════════════════════════════════════════════════

// TestFilter_DropMatchingMessages validates that matching messages are dropped.
func TestFilter_DropMatchingMessages(t *testing.T) {
	filter, err := NewDropFilter("drop-test",
		Condition{Field: "topic", Operator: OperatorContains, Value: "debug"},
	)
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}

	tests := []struct {
		name       string
		topic      string
		shouldDrop bool
	}{
		{"debug topic drops", "logs/debug", true},
		{"info topic passes", "logs/info", false},
		{"contains debug drops", "app/debug/messages", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &types.Message{Topic: tt.topic}
			nextCalled := false

			err := filter.Process(context.Background(), msg, func(ctx context.Context, m *types.Message) error {
				nextCalled = true
				return nil
			})

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if tt.shouldDrop && nextCalled {
				t.Error("expected message to be dropped, but next was called")
			}
			if !tt.shouldDrop && !nextCalled {
				t.Error("expected message to pass, but next was not called")
			}
		})
	}
}

// TestFilter_PassMatchingMessages validates that only matching messages pass.
func TestFilter_PassMatchingMessages(t *testing.T) {
	filter, err := NewPassFilter("pass-test",
		Condition{Field: "topic", Operator: OperatorEquals, Value: "sensors/temperature"},
	)
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}

	tests := []struct {
		name       string
		topic      string
		shouldPass bool
	}{
		{"exact match passes", "sensors/temperature", true},
		{"different topic drops", "sensors/humidity", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &types.Message{Topic: tt.topic}
			nextCalled := false

			err := filter.Process(context.Background(), msg, func(ctx context.Context, m *types.Message) error {
				nextCalled = true
				return nil
			})

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if tt.shouldPass && !nextCalled {
				t.Error("expected message to pass, but next was not called")
			}
			if !tt.shouldPass && nextCalled {
				t.Error("expected message to be dropped, but next was called")
			}
		})
	}
}

// TestFilter_RouteMatchingMessages validates route action sets metadata.
func TestFilter_RouteMatchingMessages(t *testing.T) {
	filter, err := NewRouteFilter("route-test", "archive-queue",
		Condition{Field: "metadata.priority", Operator: OperatorEquals, Value: "low"},
	)
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}

	msg := &types.Message{
		Topic:    "events",
		Metadata: map[string]any{"priority": "low"},
	}

	var processedMsg *types.Message
	err = filter.Process(context.Background(), msg, func(ctx context.Context, m *types.Message) error {
		processedMsg = m
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if processedMsg == nil {
		t.Fatal("expected next to be called")
	}

	routeTo, ok := processedMsg.Metadata["_routeTo"]
	if !ok {
		t.Error("expected _routeTo metadata to be set")
	}
	if routeTo != "archive-queue" {
		t.Errorf("expected routeTo=archive-queue, got %v", routeTo)
	}
}

// TestCondition_Operators validates different comparison operators.
func TestCondition_Operators(t *testing.T) {
	tests := []struct {
		name      string
		condition Condition
		message   *types.Message
		expected  bool
	}{
		{
			name:      "equals string",
			condition: Condition{Field: "topic", Operator: OperatorEquals, Value: "test"},
			message:   &types.Message{Topic: "test"},
			expected:  true,
		},
		{
			name:      "not equals",
			condition: Condition{Field: "topic", Operator: OperatorNotEquals, Value: "test"},
			message:   &types.Message{Topic: "other"},
			expected:  true,
		},
		{
			name:      "contains",
			condition: Condition{Field: "topic", Operator: OperatorContains, Value: "sensor"},
			message:   &types.Message{Topic: "sensors/temperature"},
			expected:  true,
		},
		{
			name:      "regex match",
			condition: Condition{Field: "topic", Operator: OperatorRegex, Value: "^sensors/.*"},
			message:   &types.Message{Topic: "sensors/temperature"},
			expected:  true,
		},
		{
			name:      "regex no match",
			condition: Condition{Field: "topic", Operator: OperatorRegex, Value: "^events/.*"},
			message:   &types.Message{Topic: "sensors/temperature"},
			expected:  false,
		},
		{
			name:      "greater than",
			condition: Condition{Field: "metadata.count", Operator: OperatorGreaterThan, Value: 10.0},
			message:   &types.Message{Metadata: map[string]any{"count": 15.0}},
			expected:  true,
		},
		{
			name:      "less than",
			condition: Condition{Field: "metadata.count", Operator: OperatorLessThan, Value: 10.0},
			message:   &types.Message{Metadata: map[string]any{"count": 5.0}},
			expected:  true,
		},
		{
			name:      "exists true",
			condition: Condition{Field: "metadata.flag", Operator: OperatorExists, Value: true},
			message:   &types.Message{Metadata: map[string]any{"flag": "value"}},
			expected:  true,
		},
		{
			name:      "exists false",
			condition: Condition{Field: "metadata.missing", Operator: OperatorExists, Value: true},
			message:   &types.Message{Metadata: map[string]any{"flag": "value"}},
			expected:  false,
		},
		{
			name:      "in list",
			condition: Condition{Field: "metadata.status", Operator: OperatorIn, Value: []any{"active", "pending"}},
			message:   &types.Message{Metadata: map[string]any{"status": "active"}},
			expected:  true,
		},
		{
			name:      "not in list",
			condition: Condition{Field: "metadata.status", Operator: OperatorIn, Value: []any{"active", "pending"}},
			message:   &types.Message{Metadata: map[string]any{"status": "completed"}},
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, err := New(Config{
				Conditions: []Condition{tt.condition},
				Action:     FilterActionDrop,
			})
			if err != nil {
				t.Fatalf("failed to create filter: %v", err)
			}

			match, err := filter.evaluate(tt.message)
			if err != nil {
				t.Fatalf("evaluate failed: %v", err)
			}

			if match != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, match)
			}
		})
	}
}

// TestCondition_JSONPath validates JSONPath field extraction from payload.
func TestCondition_JSONPath(t *testing.T) {
	payload := map[string]any{
		"user": map[string]any{
			"name": "alice",
			"age":  30,
		},
		"status": "active",
	}
	payloadBytes, _ := json.Marshal(payload)

	tests := []struct {
		name      string
		condition Condition
		expected  bool
	}{
		{
			name:      "simple path",
			condition: Condition{Field: "$.status", Operator: OperatorEquals, Value: "active"},
			expected:  true,
		},
		{
			name:      "nested path",
			condition: Condition{Field: "$.user.name", Operator: OperatorEquals, Value: "alice"},
			expected:  true,
		},
		{
			name:      "numeric comparison",
			condition: Condition{Field: "$.user.age", Operator: OperatorGreaterThan, Value: 25.0},
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, err := New(Config{
				Conditions: []Condition{tt.condition},
				Action:     FilterActionDrop,
			})
			if err != nil {
				t.Fatalf("failed to create filter: %v", err)
			}

			msg := &types.Message{Payload: payloadBytes}
			match, err := filter.evaluate(msg)
			if err != nil {
				t.Fatalf("evaluate failed: %v", err)
			}

			if match != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, match)
			}
		})
	}
}

// TestFilter_MultipleConditions validates AND logic for multiple conditions.
func TestFilter_MultipleConditions(t *testing.T) {
	filter, err := New(Config{
		Conditions: []Condition{
			{Field: "topic", Operator: OperatorContains, Value: "sensors"},
			{Field: "metadata.priority", Operator: OperatorEquals, Value: "high"},
		},
		Action: FilterActionPass,
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}

	tests := []struct {
		name     string
		topic    string
		metadata map[string]any
		expected bool
	}{
		{
			name:     "both match",
			topic:    "sensors/temp",
			metadata: map[string]any{"priority": "high"},
			expected: true,
		},
		{
			name:     "only topic matches",
			topic:    "sensors/temp",
			metadata: map[string]any{"priority": "low"},
			expected: false,
		},
		{
			name:     "only priority matches",
			topic:    "events/alert",
			metadata: map[string]any{"priority": "high"},
			expected: false,
		},
		{
			name:     "neither matches",
			topic:    "events/alert",
			metadata: map[string]any{"priority": "low"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &types.Message{Topic: tt.topic, Metadata: tt.metadata}
			match, err := filter.evaluate(msg)
			if err != nil {
				t.Fatalf("evaluate failed: %v", err)
			}

			if match != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, match)
			}
		})
	}
}

// TestFilter_NextError validates that errors from next are propagated.
func TestFilter_NextError(t *testing.T) {
	filter, err := New(Config{
		Action: FilterActionPass,
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}

	expectedErr := errors.New("downstream error")
	msg := &types.Message{Topic: "test"}

	err = filter.Process(context.Background(), msg, func(ctx context.Context, m *types.Message) error {
		return expectedErr
	})

	if err != expectedErr {
		t.Errorf("expected error to be propagated, got %v", err)
	}
}

// TestFilter_InvalidRegex validates that invalid regex returns error.
func TestFilter_InvalidRegex(t *testing.T) {
	_, err := New(Config{
		Conditions: []Condition{
			{Field: "topic", Operator: OperatorRegex, Value: "[invalid(regex"},
		},
		Action: FilterActionDrop,
	})

	if err == nil {
		t.Error("expected error for invalid regex")
	}
}

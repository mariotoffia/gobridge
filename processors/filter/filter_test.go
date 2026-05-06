package filter

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func nextOK(_ context.Context, _ *domain.Envelope) error { return nil }

func nextErr(sentinel error) func(context.Context, *domain.Envelope) error {
	return func(_ context.Context, _ *domain.Envelope) error { return sentinel }
}

func envelope(subject string, headers map[string]any, payload any) *domain.Envelope {
	env := &domain.Envelope{Subject: subject, Headers: headers}
	if payload != nil {
		env.Payload, _ = json.Marshal(payload)
	}
	return env
}

// Verifies drop filter drops matching messages and passes others to next.
func TestDropFilter_MatchingMessages(t *testing.T) {
	p, err := NewDropFilter("drop-test",
		Condition{Field: "subject", Operator: OperatorEquals, Value: "orders.new"},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	t.Run("matching message is dropped", func(t *testing.T) {
		env := envelope("orders.new", nil, nil)
		err := p.Process(context.Background(), env, nextOK)
		if !errors.Is(err, shared.ErrMessageFiltered) {
			t.Errorf("expected ErrMessageFiltered, got %v", err)
		}
	})

	t.Run("non-matching message calls next", func(t *testing.T) {
		env := envelope("orders.updated", nil, nil)
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})
}

// Verifies pass filter forwards matching messages and drops non-matching.
func TestPassFilter_MatchingMessages(t *testing.T) {
	p, err := NewPassFilter("pass-test",
		Condition{Field: "subject", Operator: OperatorEquals, Value: "orders.new"},
	)
	if err != nil {
		t.Fatalf("NewPassFilter: %v", err)
	}

	t.Run("matching message passes through", func(t *testing.T) {
		env := envelope("orders.new", nil, nil)
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("non-matching message is dropped", func(t *testing.T) {
		env := envelope("orders.updated", nil, nil)
		err := p.Process(context.Background(), env, nextOK)
		if !errors.Is(err, shared.ErrMessageFiltered) {
			t.Errorf("expected ErrMessageFiltered, got %v", err)
		}
	})
}

// Verifies route filter sets route header for matches and initialises nil headers.
func TestRouteFilter_SetsHeaderAndCallsNext(t *testing.T) {
	p, err := NewRouteFilter("route-test", "audit-queue",
		Condition{Field: "subject", Operator: OperatorEquals, Value: "orders.new"},
	)
	if err != nil {
		t.Fatalf("NewRouteFilter: %v", err)
	}

	t.Run("matching message sets route header", func(t *testing.T) {
		env := envelope("orders.new", map[string]any{"foo": "bar"}, nil)
		called := false
		err := p.Process(context.Background(), env, func(_ context.Context, e *domain.Envelope) error {
			called = true
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Errorf("expected next to be called")
		}
		got, ok := env.Headers[domain.HeaderRouteOverride]
		if !ok {
			t.Fatalf("route header not set")
		}
		if got != "audit-queue" {
			t.Errorf("route header = %q, want %q", got, "audit-queue")
		}
	})

	t.Run("nil headers map is initialised", func(t *testing.T) {
		env := envelope("orders.new", nil, nil)
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if env.Headers == nil {
			t.Fatal("expected headers to be initialised")
		}
		if env.Headers[domain.HeaderRouteOverride] != "audit-queue" {
			t.Errorf("route header = %v, want %q", env.Headers[domain.HeaderRouteOverride], "audit-queue")
		}
	})

	t.Run("non-matching message passes without route header", func(t *testing.T) {
		env := envelope("orders.updated", nil, nil)
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if env.Headers != nil {
			if _, ok := env.Headers[domain.HeaderRouteOverride]; ok {
				t.Errorf("route header should not be set for non-matching message")
			}
		}
	})
}

// Verifies all condition operators for drop matching against field values.
func TestCondition_AllOperators(t *testing.T) {
	tests := []struct {
		name     string
		cond     Condition
		headers  map[string]any
		wantDrop bool
	}{
		{
			name:     "eq match",
			cond:     Condition{Field: "status", Operator: OperatorEquals, Value: "active"},
			headers:  map[string]any{"status": "active"},
			wantDrop: true,
		},
		{
			name:     "eq no match",
			cond:     Condition{Field: "status", Operator: OperatorEquals, Value: "active"},
			headers:  map[string]any{"status": "inactive"},
			wantDrop: false,
		},
		{
			name:     "ne match",
			cond:     Condition{Field: "status", Operator: OperatorNotEquals, Value: "active"},
			headers:  map[string]any{"status": "inactive"},
			wantDrop: true,
		},
		{
			name:     "ne no match",
			cond:     Condition{Field: "status", Operator: OperatorNotEquals, Value: "active"},
			headers:  map[string]any{"status": "active"},
			wantDrop: false,
		},
		{
			name:     "contains match",
			cond:     Condition{Field: "path", Operator: OperatorContains, Value: "admin"},
			headers:  map[string]any{"path": "/api/admin/users"},
			wantDrop: true,
		},
		{
			name:     "contains no match",
			cond:     Condition{Field: "path", Operator: OperatorContains, Value: "admin"},
			headers:  map[string]any{"path": "/api/public/health"},
			wantDrop: false,
		},
		{
			name:     "regex match",
			cond:     Condition{Field: "id", Operator: OperatorRegex, Value: `^order-\d+$`},
			headers:  map[string]any{"id": "order-123"},
			wantDrop: true,
		},
		{
			name:     "regex no match",
			cond:     Condition{Field: "id", Operator: OperatorRegex, Value: `^order-\d+$`},
			headers:  map[string]any{"id": "user-abc"},
			wantDrop: false,
		},
		{
			name:     "gt match",
			cond:     Condition{Field: "priority", Operator: OperatorGreaterThan, Value: 5},
			headers:  map[string]any{"priority": 10},
			wantDrop: true,
		},
		{
			name:     "gt no match",
			cond:     Condition{Field: "priority", Operator: OperatorGreaterThan, Value: 5},
			headers:  map[string]any{"priority": 3},
			wantDrop: false,
		},
		{
			name:     "lt match",
			cond:     Condition{Field: "priority", Operator: OperatorLessThan, Value: 5},
			headers:  map[string]any{"priority": 3},
			wantDrop: true,
		},
		{
			name:     "lt no match",
			cond:     Condition{Field: "priority", Operator: OperatorLessThan, Value: 5},
			headers:  map[string]any{"priority": 10},
			wantDrop: false,
		},
		{
			name:     "gte match equal",
			cond:     Condition{Field: "priority", Operator: OperatorGreaterOrEqual, Value: 5},
			headers:  map[string]any{"priority": 5},
			wantDrop: true,
		},
		{
			name:     "gte match greater",
			cond:     Condition{Field: "priority", Operator: OperatorGreaterOrEqual, Value: 5},
			headers:  map[string]any{"priority": 6},
			wantDrop: true,
		},
		{
			name:     "gte no match",
			cond:     Condition{Field: "priority", Operator: OperatorGreaterOrEqual, Value: 5},
			headers:  map[string]any{"priority": 4},
			wantDrop: false,
		},
		{
			name:     "lte match equal",
			cond:     Condition{Field: "priority", Operator: OperatorLessOrEqual, Value: 5},
			headers:  map[string]any{"priority": 5},
			wantDrop: true,
		},
		{
			name:     "lte match less",
			cond:     Condition{Field: "priority", Operator: OperatorLessOrEqual, Value: 5},
			headers:  map[string]any{"priority": 4},
			wantDrop: true,
		},
		{
			name:     "lte no match",
			cond:     Condition{Field: "priority", Operator: OperatorLessOrEqual, Value: 5},
			headers:  map[string]any{"priority": 6},
			wantDrop: false,
		},
		{
			name:     "exists true and field present",
			cond:     Condition{Field: "tag", Operator: OperatorExists, Value: true},
			headers:  map[string]any{"tag": "v1"},
			wantDrop: true,
		},
		{
			name:     "exists true and field absent",
			cond:     Condition{Field: "tag", Operator: OperatorExists, Value: true},
			headers:  map[string]any{},
			wantDrop: false,
		},
		{
			name:     "exists false and field absent",
			cond:     Condition{Field: "tag", Operator: OperatorExists, Value: false},
			headers:  map[string]any{},
			wantDrop: true,
		},
		{
			name:     "in match",
			cond:     Condition{Field: "env", Operator: OperatorIn, Value: []any{"prod", "staging"}},
			headers:  map[string]any{"env": "prod"},
			wantDrop: true,
		},
		{
			name:     "in no match",
			cond:     Condition{Field: "env", Operator: OperatorIn, Value: []any{"prod", "staging"}},
			headers:  map[string]any{"env": "dev"},
			wantDrop: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewDropFilter("op-test", tc.cond)
			if err != nil {
				t.Fatalf("NewDropFilter: %v", err)
			}
			env := envelope("test", tc.headers, nil)
			err = p.Process(context.Background(), env, nextOK)
			dropped := errors.Is(err, shared.ErrMessageFiltered)
			if dropped != tc.wantDrop {
				t.Errorf("dropped=%v, want %v (err=%v)", dropped, tc.wantDrop, err)
			}
		})
	}
}

// Verifies JSONPath fields evaluate against payload.
func TestCondition_JSONPath(t *testing.T) {
	payload := map[string]any{
		"order": map[string]any{
			"total":  99.95,
			"status": "confirmed",
		},
	}

	tests := []struct {
		name     string
		field    string
		op       Operator
		value    any
		wantDrop bool
	}{
		{"eq on nested string", "$.order.status", OperatorEquals, "confirmed", true},
		{"gt on nested number", "$.order.total", OperatorGreaterThan, 50.0, true},
		{"lt on nested number", "$.order.total", OperatorLessThan, 50.0, false},
		{"missing path", "$.order.missing", OperatorEquals, "x", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewDropFilter("jsonpath-test",
				Condition{Field: tc.field, Operator: tc.op, Value: tc.value},
			)
			if err != nil {
				t.Fatalf("NewDropFilter: %v", err)
			}
			env := envelope("test", nil, payload)
			err = p.Process(context.Background(), env, nextOK)
			dropped := errors.Is(err, shared.ErrMessageFiltered)
			if dropped != tc.wantDrop {
				t.Errorf("dropped=%v, want %v (err=%v)", dropped, tc.wantDrop, err)
			}
		})
	}
}

// Verifies subject field matching in conditions.
func TestCondition_SubjectField(t *testing.T) {
	p, err := NewDropFilter("subject-test",
		Condition{Field: "subject", Operator: OperatorContains, Value: "orders"},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := envelope("orders.created", nil, nil)
	err = p.Process(context.Background(), env, nextOK)
	if !errors.Is(err, shared.ErrMessageFiltered) {
		t.Errorf("expected ErrMessageFiltered for subject containing 'orders', got %v", err)
	}

	env = envelope("users.updated", nil, nil)
	err = p.Process(context.Background(), env, nextOK)
	if err != nil {
		t.Errorf("expected nil for non-matching subject, got %v", err)
	}
}

// Verifies header.x field syntax matches envelope headers.
func TestCondition_HeaderField(t *testing.T) {
	p, err := NewDropFilter("header-test",
		Condition{Field: "header.x-custom", Operator: OperatorEquals, Value: "secret"},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := envelope("test", map[string]any{"x-custom": "secret"}, nil)
	err = p.Process(context.Background(), env, nextOK)
	if !errors.Is(err, shared.ErrMessageFiltered) {
		t.Errorf("expected ErrMessageFiltered, got %v", err)
	}

	env = envelope("test", map[string]any{"x-custom": "public"}, nil)
	err = p.Process(context.Background(), env, nextOK)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// Verifies bare field names fall back to header lookup.
func TestCondition_BareFieldFallback(t *testing.T) {
	p, err := NewDropFilter("bare-test",
		Condition{Field: "tenant", Operator: OperatorEquals, Value: "acme"},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := envelope("test", map[string]any{"tenant": "acme"}, nil)
	err = p.Process(context.Background(), env, nextOK)
	if !errors.Is(err, shared.ErrMessageFiltered) {
		t.Errorf("expected ErrMessageFiltered for bare field matching header, got %v", err)
	}

	env = envelope("test", map[string]any{"tenant": "other"}, nil)
	err = p.Process(context.Background(), env, nextOK)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// Verifies AND semantics: all conditions must match for drop.
func TestFilter_MultipleConditions(t *testing.T) {
	p, err := NewDropFilter("multi-test",
		Condition{Field: "subject", Operator: OperatorEquals, Value: "orders.new"},
		Condition{Field: "header.priority", Operator: OperatorEquals, Value: "high"},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	t.Run("all conditions match", func(t *testing.T) {
		env := envelope("orders.new", map[string]any{"priority": "high"}, nil)
		err := p.Process(context.Background(), env, nextOK)
		if !errors.Is(err, shared.ErrMessageFiltered) {
			t.Errorf("expected ErrMessageFiltered, got %v", err)
		}
	})

	t.Run("first condition fails", func(t *testing.T) {
		env := envelope("orders.updated", map[string]any{"priority": "high"}, nil)
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Errorf("expected nil when first condition fails, got %v", err)
		}
	})

	t.Run("second condition fails", func(t *testing.T) {
		env := envelope("orders.new", map[string]any{"priority": "low"}, nil)
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Errorf("expected nil when second condition fails, got %v", err)
		}
	})
}

// Verifies Invert flips match semantics for the filter action.
func TestFilter_Inversion(t *testing.T) {
	p, err := New(Config{
		Name:       "invert-test",
		Conditions: []Condition{{Field: "subject", Operator: OperatorEquals, Value: "orders.new"}},
		Action:     ActionDrop,
		Invert:     true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Run("matching becomes non-matching with invert", func(t *testing.T) {
		env := envelope("orders.new", nil, nil)
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Errorf("expected nil (inverted match should pass), got %v", err)
		}
	})

	t.Run("non-matching becomes matching with invert", func(t *testing.T) {
		env := envelope("orders.updated", nil, nil)
		err := p.Process(context.Background(), env, nextOK)
		if !errors.Is(err, shared.ErrMessageFiltered) {
			t.Errorf("expected ErrMessageFiltered (inverted non-match should drop), got %v", err)
		}
	})
}

// Verifies errors from next propagate when the message passes the filter.
func TestFilter_NextErrorPropagation(t *testing.T) {
	p, err := New(Config{
		Name:   "propagation-test",
		Action: ActionPass,
		Conditions: []Condition{
			{Field: "subject", Operator: OperatorEquals, Value: "test"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sentinel := errors.New("downstream failure")
	env := envelope("test", nil, nil)
	err = p.Process(context.Background(), env, nextErr(sentinel))
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// Verifies New rejects invalid regex patterns.
func TestFilter_InvalidRegex(t *testing.T) {
	_, err := New(Config{
		Conditions: []Condition{
			{Field: "subject", Operator: OperatorRegex, Value: "[invalid("},
		},
		Action: ActionDrop,
	})
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
}

// Verifies route action requires RouteTo.
func TestFilter_RouteRequiresRouteTo(t *testing.T) {
	_, err := New(Config{
		Conditions: []Condition{
			{Field: "subject", Operator: OperatorEquals, Value: "x"},
		},
		Action: ActionRoute,
	})
	if !errors.Is(err, ErrRouteRequired) {
		t.Errorf("expected ErrRouteRequired, got %v", err)
	}
}

// Verifies processor Name default and override.
func TestProcessor_Name(t *testing.T) {
	t.Run("configured name", func(t *testing.T) {
		p, err := New(Config{Name: "my-filter", Action: ActionPass})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := p.Name(); got != "my-filter" {
			t.Errorf("Name() = %q, want %q", got, "my-filter")
		}
	})

	t.Run("default name", func(t *testing.T) {
		p, err := New(Config{Action: ActionPass})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := p.Name(); got != "filter" {
			t.Errorf("Name() = %q, want %q", got, "filter")
		}
	})
}

// Verifies an empty condition list always matches.
func TestFilter_NoConditionsAlwaysMatches(t *testing.T) {
	p, err := New(Config{Action: ActionDrop})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	env := envelope("anything", nil, nil)
	err = p.Process(context.Background(), env, nextOK)
	if !errors.Is(err, shared.ErrMessageFiltered) {
		t.Errorf("expected ErrMessageFiltered (no conditions = always match + drop), got %v", err)
	}
}

// Verifies nil headers do not panic for header-prefixed and bare fields.
func TestFilter_NilHeaders(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{"header. prefix with nil headers", "header.x-key"},
		{"bare field with nil headers", "tenant"},
		{"exists on nil headers", "missing"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewDropFilter("nil-hdr",
				Condition{Field: tc.field, Operator: OperatorEquals, Value: "val"},
			)
			if err != nil {
				t.Fatalf("NewDropFilter: %v", err)
			}
			env := &domain.Envelope{Subject: "test"}
			err = p.Process(context.Background(), env, nextOK)
			if err != nil {
				t.Errorf("expected nil (no panic, field not found = no match), got %v", err)
			}
		})
	}
}

// Verifies JSONPath conditions do not match on nil or empty payload.
func TestFilter_EmptyPayload(t *testing.T) {
	p, err := NewDropFilter("empty-payload",
		Condition{Field: "$.order.id", Operator: OperatorEquals, Value: "123"},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	t.Run("nil payload", func(t *testing.T) {
		env := &domain.Envelope{Subject: "test"}
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Errorf("expected nil (no panic, empty payload = no match), got %v", err)
		}
	})

	t.Run("empty byte slice payload", func(t *testing.T) {
		env := &domain.Envelope{Subject: "test", Payload: []byte{}}
		err := p.Process(context.Background(), env, nextOK)
		if err != nil {
			t.Errorf("expected nil (no panic, empty payload = no match), got %v", err)
		}
	})
}

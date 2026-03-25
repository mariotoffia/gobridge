package runtime_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/runtime"
)

// Verifies routing with a nil DLQ store is a no-op and returns no error.
func TestDLQRouter_NilStore(t *testing.T) {
	dlq := runtime.NewDLQRouter(nil)
	env := &domain.Envelope{ID: "msg-1"}

	err := dlq.Route(context.Background(), env, "route-1", "", "", "", domain.ErrNotFound, 1)
	if err != nil {
		t.Fatalf("nil store should be a no-op, got %v", err)
	}
}

// Verifies Route persists one entry with route, correlation, permanent category, and attempt count.
func TestDLQRouter_WritesEntry(t *testing.T) {
	store := NewFakeDLQStore()
	dlq := runtime.NewDLQRouter(store)

	env := &domain.Envelope{
		ID:      "msg-1",
		Headers: map[string]any{domain.HeaderCorrelationID: "corr-abc"},
	}

	err := dlq.Route(context.Background(), env, "route-1", "bind-1", "sess-1", "src-1", domain.ErrNotFound, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", store.Count())
	}

	entry := store.Entries[0]
	if entry.RouteID != "route-1" {
		t.Fatalf("expected route 'route-1', got %q", entry.RouteID)
	}
	if entry.CorrelationID != "corr-abc" {
		t.Fatalf("expected correlation ID 'corr-abc', got %q", entry.CorrelationID)
	}
	if entry.Category != string(domain.ErrorPermanent) {
		t.Fatalf("expected category 'permanent', got %q", entry.Category)
	}
	if entry.Attempts != 3 {
		t.Fatalf("expected attempts 3, got %d", entry.Attempts)
	}
}

// Verifies non-domain errors are categorized as unknown in the DLQ entry.
func TestDLQRouter_UnknownError(t *testing.T) {
	store := NewFakeDLQStore()
	dlq := runtime.NewDLQRouter(store)

	env := &domain.Envelope{ID: "msg-1"}
	_ = dlq.Route(context.Background(), env, "r", "", "", "", context.DeadlineExceeded, 0)

	if store.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Count())
	}
	if store.Entries[0].Category != "unknown" {
		t.Fatalf("expected category 'unknown', got %q", store.Entries[0].Category)
	}
}

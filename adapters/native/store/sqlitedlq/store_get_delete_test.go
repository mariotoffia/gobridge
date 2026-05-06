package sqlitedlq_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ═══════════════════════════════════════════════════════════════════
// Get / Delete / DeleteByFilter — targeted edge-case tests for SQLite
// ═══════════════════════════════════════════════════════════════════

func newMemDB(t *testing.T) *sqlitedlq.Store {
	t.Helper()
	s, err := sqlitedlq.NewStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeEntry(t *testing.T, s *sqlitedlq.Store, id, route, cat string, failedAt time.Time) {
	t.Helper()
	if err := s.Write(context.Background(), routing.DLQEntry{
		ID: id, RouteID: route, Category: cat, FailedAt: failedAt,
		Envelope: messaging.Envelope{ID: "env-" + id, Subject: "test"},
	}); err != nil {
		t.Fatalf("write %s: %v", id, err)
	}
}

// TestGet_Existing_ReturnsFullEntry validates Get returns the
// complete entry including envelope with headers.
func TestGet_Existing_ReturnsFullEntry(t *testing.T) {
	s := newMemDB(t)
	ctx := context.Background()

	entry := routing.DLQEntry{
		ID: "sg-1", RouteID: "route-g", Category: "timeout",
		Envelope: messaging.Envelope{
			ID:      "env-sg-1",
			Subject: "test/get",
			Payload: []byte(`{"k":"v"}`),
			Headers: map[string]any{"h": "v"},
		},
		FailedAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		Attempts: 2,
	}
	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := s.Get(ctx, "sg-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "sg-1" {
		t.Errorf("ID: got %q, want %q", got.ID, "sg-1")
	}
	if got.Envelope.ID != "env-sg-1" {
		t.Errorf("Envelope.ID: got %q, want %q", got.Envelope.ID, "env-sg-1")
	}
	if string(got.Envelope.Payload) != `{"k":"v"}` {
		t.Errorf("Payload: got %q", got.Envelope.Payload)
	}
}

// TestGet_Missing_ReturnsErrNotFound validates ErrNotFound on missing ID.
func TestGet_Missing_ReturnsErrNotFound(t *testing.T) {
	s := newMemDB(t)
	_, err := s.Get(context.Background(), "no-such")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestDeleteByFilter_ByRouteID validates route-scoped deletion.
func TestDeleteByFilter_ByRouteID(t *testing.T) {
	s := newMemDB(t)
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	writeEntry(t, s, "sdf-r1", "route-A", "timeout", base)
	writeEntry(t, s, "sdf-r2", "route-A", "timeout", base.Add(time.Hour))
	writeEntry(t, s, "sdf-r3", "route-B", "timeout", base.Add(2*time.Hour))

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{RouteID: "route-A"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	remaining, _ := s.List(ctx, routing.DLQFilter{})
	if len(remaining) != 1 || remaining[0].ID != "sdf-r3" {
		t.Fatalf("unexpected remaining: %v", remaining)
	}
}

// TestDeleteByFilter_ByCategory validates category-scoped deletion.
func TestDeleteByFilter_ByCategory(t *testing.T) {
	s := newMemDB(t)
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	writeEntry(t, s, "sdf-c1", "route-A", "timeout", base)
	writeEntry(t, s, "sdf-c2", "route-A", "rejected", base.Add(time.Hour))

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{Category: "timeout"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted, got %d", n)
	}
}

// TestDeleteByFilter_TimeRange validates half-open [Since, Before) range.
func TestDeleteByFilter_TimeRange(t *testing.T) {
	s := newMemDB(t)
	ctx := context.Background()

	t1 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(1 * time.Hour)
	t3 := t1.Add(2 * time.Hour)
	t4 := t1.Add(3 * time.Hour)

	writeEntry(t, s, "sdf-t1", "route-A", "timeout", t1)
	writeEntry(t, s, "sdf-t2", "route-A", "timeout", t2)
	writeEntry(t, s, "sdf-t3", "route-A", "timeout", t3)
	writeEntry(t, s, "sdf-t4", "route-A", "timeout", t4)

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{Since: t2, Before: t4})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	remaining, _ := s.List(ctx, routing.DLQFilter{})
	if len(remaining) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(remaining))
	}
}

// TestDeleteByFilter_WithLimit validates subquery-based LIMIT.
func TestDeleteByFilter_WithLimit(t *testing.T) {
	s := newMemDB(t)
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	for i := range 5 {
		writeEntry(t, s, "sdf-l"+itoaS(i+1), "route-A", "timeout",
			base.Add(time.Duration(i)*time.Hour))
	}

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{RouteID: "route-A", Limit: 2})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	remaining, _ := s.List(ctx, routing.DLQFilter{RouteID: "route-A"})
	if len(remaining) != 3 {
		t.Fatalf("expected 3 remaining, got %d", len(remaining))
	}
}

// TestDeleteByFilter_EmptyFilter validates all entries are removed.
func TestDeleteByFilter_EmptyFilter(t *testing.T) {
	s := newMemDB(t)
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	writeEntry(t, s, "sdf-e1", "route-A", "timeout", base)
	writeEntry(t, s, "sdf-e2", "route-B", "rejected", base.Add(time.Hour))

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}
}

// TestDeleteByFilter_CombinedFilters validates route + category +
// time range applied together.
func TestDeleteByFilter_CombinedFilters(t *testing.T) {
	s := newMemDB(t)
	ctx := context.Background()

	t1 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(1 * time.Hour)
	t3 := t1.Add(2 * time.Hour)

	writeEntry(t, s, "sdf-m1", "route-A", "timeout", t1)  // no: before Since
	writeEntry(t, s, "sdf-m2", "route-A", "timeout", t2)  // yes: matches all
	writeEntry(t, s, "sdf-m3", "route-A", "rejected", t2) // no: wrong category
	writeEntry(t, s, "sdf-m4", "route-B", "timeout", t2)  // no: wrong route

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{
		RouteID:  "route-A",
		Category: "timeout",
		Since:    t2,
		Before:   t3,
	})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted, got %d", n)
	}
}

func itoaS(n int) string {
	return strconv.Itoa(n)
}

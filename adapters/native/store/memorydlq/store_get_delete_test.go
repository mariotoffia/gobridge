package memorydlq_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ═══════════════════════════════════════════════════════════════════
// Get / Delete / DeleteByFilter — targeted edge-case tests
//
// The conformance suite covers basic behavior; these tests cover
// store-specific edge cases and document the contract precisely.
// ═══════════════════════════════════════════════════════════════════

// TestGet_Existing_ReturnsFullEntry validates Get returns the complete
// entry including all envelope fields.
func TestGet_Existing_ReturnsFullEntry(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	entry := routing.DLQEntry{
		ID:      "g-1",
		RouteID: "route-g",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-g-1",
			Subject: "test/get",
			Payload: []byte(`{"data":"value"}`),
			Headers: map[string]any{"h": "v"},
		}),
		FailedAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		Category: "timeout",
		Attempts: 3,
	}
	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := s.Get(ctx, "g-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "g-1" {
		t.Errorf("ID: got %q, want %q", got.ID, "g-1")
	}
	if got.Envelope.ID != "env-g-1" {
		t.Errorf("Envelope.ID: got %q, want %q", got.Envelope.ID, "env-g-1")
	}
	if string(got.Envelope.Payload) != `{"data":"value"}` {
		t.Errorf("Envelope.Payload: got %q", got.Envelope.Payload)
	}
	if got.Envelope.Headers()["h"] != "v" {
		t.Errorf("Envelope.Headers[h]: got %v", got.Envelope.Headers()["h"])
	}
}

// TestGet_Missing_ReturnsErrNotFound validates Get returns
// shared.ErrNotFound for nonexistent IDs.
func TestGet_Missing_ReturnsErrNotFound(t *testing.T) {
	s := memorydlq.NewStore()
	_, err := s.Get(context.Background(), "nope")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestDeleteByFilter_ByRouteID validates only entries for the specified
// route are deleted.
func TestDeleteByFilter_ByRouteID(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	write(t, s, "df-r1", "route-A", "timeout", time.Now())
	write(t, s, "df-r2", "route-A", "timeout", time.Now().Add(time.Hour))
	write(t, s, "df-r3", "route-B", "timeout", time.Now().Add(2*time.Hour))

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{RouteID: "route-A"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	remaining, _ := s.List(ctx, routing.DLQFilter{})
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(remaining))
	}
	if remaining[0].ID != "df-r3" {
		t.Fatalf("remaining: got %q, want df-r3", remaining[0].ID)
	}
}

// TestDeleteByFilter_ByCategory validates category-scoped deletion.
func TestDeleteByFilter_ByCategory(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	write(t, s, "df-c1", "route-A", "timeout", time.Now())
	write(t, s, "df-c2", "route-A", "rejected", time.Now().Add(time.Hour))

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{Category: "timeout"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted, got %d", n)
	}
}

// TestDeleteByFilter_WithLimit validates the limit cap on deletions.
// Entries are sorted by FailedAt descending (newest first) before
// the limit is applied, matching SQLite and DynamoDB behavior.
func TestDeleteByFilter_WithLimit(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	for i := range 5 {
		write(t, s, "df-l"+itoa(i+1), "route-A", "timeout",
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

// TestDeleteByFilter_EmptyFilter validates all entries are removed
// when no filter criteria are specified.
func TestDeleteByFilter_EmptyFilter(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	write(t, s, "df-e1", "route-A", "timeout", time.Now())
	write(t, s, "df-e2", "route-B", "rejected", time.Now().Add(time.Hour))

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	remaining, _ := s.List(ctx, routing.DLQFilter{})
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining, got %d", len(remaining))
	}
}

// TestDeleteByFilter_NoMatch validates 0 returned when nothing matches.
func TestDeleteByFilter_NoMatch(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	write(t, s, "df-n1", "route-A", "timeout", time.Now())

	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{RouteID: "route-NOPE"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 deleted, got %d", n)
	}
}

// TestDeleteByFilter_BySinceAndBefore validates the half-open time
// range [Since, Before) filter.
func TestDeleteByFilter_BySinceAndBefore(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	t1 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(1 * time.Hour)
	t3 := t1.Add(2 * time.Hour)
	t4 := t1.Add(3 * time.Hour)

	write(t, s, "df-t1", "route-A", "timeout", t1)
	write(t, s, "df-t2", "route-A", "timeout", t2)
	write(t, s, "df-t3", "route-A", "timeout", t3)
	write(t, s, "df-t4", "route-A", "timeout", t4)

	// Delete [t2, t4) → should remove df-t2 and df-t3
	n, err := s.DeleteByFilter(ctx, routing.DLQFilter{Since: t2, Before: t4})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}
}

func write(t *testing.T, s *memorydlq.Store, id, route, cat string, failedAt time.Time) {
	t.Helper()
	if err := s.Write(context.Background(), routing.DLQEntry{
		ID: id, RouteID: route, Category: cat, FailedAt: failedAt,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-" + id, Subject: "test"}),
	}); err != nil {
		t.Fatalf("write %s: %v", id, err)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

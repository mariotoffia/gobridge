package dynamodbdlq_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// ═══════════════════════════════════════════════════════════════════
// Get / Delete / DeleteByFilter — targeted integration tests
//
// These tests require DynamoDB Local running via Docker.
// The conformance suite covers basic behavior; these focus on
// DynamoDB-specific edge cases and a known KeyConditionExpression bug.
// ═══════════════════════════════════════════════════════════════════

func newTestStore(t *testing.T, prefix string) *dynamodbdlq.Store {
	t.Helper()
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable(prefix)
	store := dynamodbdlq.NewStore(client, dynamodbdlq.WithTableName(tableName))
	if err := store.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)
	return store
}

func writeTestEntry(t *testing.T, store *dynamodbdlq.Store, id, route, cat string, failedAt time.Time) {
	t.Helper()
	if err := store.Write(context.Background(), routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: id, RouteID: route, Category: cat, FailedAt: failedAt,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-" + id, Subject: "test"}),
	})); err != nil {
		t.Fatalf("write %s: %v", id, err)
	}
}

// TestGet_Existing_ReturnsFullEntry validates Get returns the complete
// entry via DynamoDB GetItem, including envelope data.
func TestGet_Existing_ReturnsFullEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DynamoDB Local")
	}
	store := newTestStore(t, "dlq-get")
	ctx := context.Background()

	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "dg-1", RouteID: "route-g", Category: "timeout",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-dg-1",
			Subject: "test/get",
			Payload: []byte(`{"k":"v"}`),
			Headers: map[string]any{"h": "v"},
		}),
		FailedAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		Attempts: 2,
	})
	if err := store.Write(ctx, entry); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := store.Get(ctx, "dg-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID() != "dg-1" {
		t.Errorf("ID: got %q, want %q", got.ID(), "dg-1")
	}
	genv := got.Snapshot()
	if genv.ID() != "env-dg-1" {
		t.Errorf("Envelope.ID: got %q, want %q", genv.ID(), "env-dg-1")
	}
	if string(genv.Payload()) != `{"k":"v"}` {
		t.Errorf("Payload: got %q", genv.Payload())
	}
}

// TestGet_Missing_ReturnsErrNotFound validates ErrNotFound on missing ID.
func TestGet_Missing_ReturnsErrNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DynamoDB Local")
	}
	store := newTestStore(t, "dlq-getmiss")
	_, err := store.Get(context.Background(), "no-such-id")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestDeleteByFilter_ByRouteID validates route-scoped DeleteByFilter
// via List→Delete pattern used by DynamoDB implementation.
func TestDeleteByFilter_ByRouteID(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DynamoDB Local")
	}
	store := newTestStore(t, "dlq-dbfr")
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	writeTestEntry(t, store, "dbf-r1", "route-A", "timeout", base)
	writeTestEntry(t, store, "dbf-r2", "route-A", "timeout", base.Add(time.Hour))
	writeTestEntry(t, store, "dbf-r3", "route-B", "timeout", base.Add(2*time.Hour))

	n, err := store.DeleteByFilter(ctx, routing.DLQFilter{RouteID: "route-A"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	remaining, _ := store.List(ctx, routing.DLQFilter{})
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(remaining))
	}
	if remaining[0].ID() != "dbf-r3" {
		t.Errorf("remaining: got %q, want dbf-r3", remaining[0].ID())
	}
}

// TestDeleteByFilter_ByCategory validates category-scoped deletion
// via CategoryIndex GSI.
func TestDeleteByFilter_ByCategory(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DynamoDB Local")
	}
	store := newTestStore(t, "dlq-dbfc")
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	writeTestEntry(t, store, "dbf-c1", "route-A", "timeout", base)
	writeTestEntry(t, store, "dbf-c2", "route-A", "rejected", base.Add(time.Hour))
	writeTestEntry(t, store, "dbf-c3", "route-A", "timeout", base.Add(2*time.Hour))

	n, err := store.DeleteByFilter(ctx, routing.DLQFilter{Category: "timeout"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}
}

// TestDeleteByFilter_EmptyFilter validates full table deletion when
// no filter criteria are specified (uses scan path).
func TestDeleteByFilter_EmptyFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DynamoDB Local")
	}
	store := newTestStore(t, "dlq-dbfe")
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	writeTestEntry(t, store, "dbf-e1", "route-A", "timeout", base)
	writeTestEntry(t, store, "dbf-e2", "route-B", "rejected", base.Add(time.Hour))

	n, err := store.DeleteByFilter(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	remaining, _ := store.List(ctx, routing.DLQFilter{})
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining, got %d", len(remaining))
	}
}

// ═══════════════════════════════════════════════════════════════════
// Regression: DynamoDB KeyConditionExpression with Since+Before
//
// DynamoDB's KeyConditionExpression supports at most one comparison
// on the sort key. When both Since and Before are set, the >= :since
// condition is in the key expression (efficient index scan) and the
// < :before condition is in the FilterExpression (post-scan filter).
// ═══════════════════════════════════════════════════════════════════

// TestListByRouteIndex_SinceAndBefore verifies that querying a GSI
// with both Since and Before correctly returns the half-open range
// [Since, Before). This is a regression test for a bug where two
// sort key comparisons were placed in KeyConditionExpression, which
// DynamoDB rejects with ValidationException.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	4 entries for same route at T1, T2, T3, T4
//	List with RouteID + Since=T2 + Before=T4
//	Expected: entries at T2 and T3 (half-open range)
//
// ───────────────────────────────────────────────
func TestListByRouteIndex_SinceAndBefore(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DynamoDB Local")
	}
	store := newTestStore(t, "dlq-kce-reg")
	ctx := context.Background()

	t1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	t4 := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)

	for _, e := range []routing.DLQEntry{
		makeEntry("kce-1", "route-kce-reg", "timeout", t1),
		makeEntry("kce-2", "route-kce-reg", "timeout", t2),
		makeEntry("kce-3", "route-kce-reg", "timeout", t3),
		makeEntry("kce-4", "route-kce-reg", "timeout", t4),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID(), err)
		}
	}

	entries, err := store.List(ctx, routing.DLQFilter{
		RouteID: "route-kce-reg",
		Since:   t2,
		Before:  t4,
	})
	if err != nil {
		t.Fatalf("list with Since+Before on RouteIndex: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries in [T2,T4), got %d", len(entries))
	}

	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.ID()] = true
	}
	if !ids["kce-2"] {
		t.Error("expected kce-2 in results")
	}
	if !ids["kce-3"] {
		t.Error("expected kce-3 in results")
	}
	if ids["kce-1"] {
		t.Error("kce-1 should NOT be in results (before Since)")
	}
	if ids["kce-4"] {
		t.Error("kce-4 should NOT be in results (at or after Before)")
	}
}

// TestListByCategoryIndex_SinceAndBefore verifies the same
// Since+Before half-open range query works on the CategoryIndex GSI.
func TestListByCategoryIndex_SinceAndBefore(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DynamoDB Local")
	}
	store := newTestStore(t, "dlq-kce-cat")
	ctx := context.Background()

	t1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	t4 := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)

	for _, e := range []routing.DLQEntry{
		makeEntry("cat-1", "route-X", "timeout", t1),
		makeEntry("cat-2", "route-Y", "timeout", t2),
		makeEntry("cat-3", "route-Z", "timeout", t3),
		makeEntry("cat-4", "route-W", "timeout", t4),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID(), err)
		}
	}

	// Category-only filter → CategoryIndex GSI path
	entries, err := store.List(ctx, routing.DLQFilter{
		Category: "timeout",
		Since:    t2,
		Before:   t4,
	})
	if err != nil {
		t.Fatalf("list with Since+Before on CategoryIndex: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries in [T2,T4), got %d", len(entries))
	}

	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.ID()] = true
	}
	if !ids["cat-2"] {
		t.Error("expected cat-2 in results")
	}
	if !ids["cat-3"] {
		t.Error("expected cat-3 in results")
	}
}

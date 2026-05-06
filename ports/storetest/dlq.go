package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	dlqT1 = time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	dlqT2 = time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC)
	dlqT3 = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	dlqT4 = time.Date(2024, 1, 1, 13, 0, 0, 0, time.UTC)
	dlqT5 = time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
)

func makeDLQEntry(id, routeID, category string, failedAt time.Time) domain.DLQEntry {
	return domain.DLQEntry{
		ID:            id,
		RouteID:       routeID,
		BindingID:     "bind-" + id,
		SessionID:     "sess-" + id,
		SourceID:      "src-" + id,
		CorrelationID: "corr-" + id,
		Reason:        "test failure",
		Category:      category,
		ErrorCode:     "TEST_ERROR",
		LastError:     "something went wrong",
		FailedAt:      failedAt,
		Attempts:      1,
		Envelope: domain.Envelope{
			ID:      "env-" + id,
			Subject: "test/subject",
			Payload: []byte(`{"key":"value"}`),
			Headers: map[string]any{"trace": "tid-" + id},
		},
	}
}

func dlqEntryIDs(entries []domain.DLQEntry) map[string]bool {
	m := make(map[string]bool, len(entries))
	for _, e := range entries {
		m[e.ID] = true
	}
	return m
}

// RunDLQStoreTests executes the full conformance suite against the given
// DLQStore. The caller is responsible for creating and closing the store.
func RunDLQStoreTests(t *testing.T, store ports.DLQStore) {
	t.Helper()
	t.Run("WriteAndList", func(t *testing.T) { dlqWriteAndList(t, store) })
	t.Run("ListFilterByRouteID", func(t *testing.T) { dlqListFilterByRouteID(t, store) })
	t.Run("ListFilterByCategory", func(t *testing.T) { dlqListFilterByCategory(t, store) })
	t.Run("ListFilterBySince", func(t *testing.T) { dlqListFilterBySince(t, store) })
	t.Run("ListFilterByBefore", func(t *testing.T) { dlqListFilterByBefore(t, store) })
	t.Run("ListRespectsLimit", func(t *testing.T) { dlqListRespectsLimit(t, store) })
	t.Run("WriteIdempotent", func(t *testing.T) { dlqWriteIdempotent(t, store) })
	t.Run("GetExisting", func(t *testing.T) { dlqGetExisting(t, store) })
	t.Run("GetMissing", func(t *testing.T) { dlqGetMissing(t, store) })
	t.Run("DeleteByIDs", func(t *testing.T) { dlqDeleteByIDs(t, store) })
	t.Run("DeleteMissing", func(t *testing.T) { dlqDeleteMissing(t, store) })
	t.Run("DeletePartialMatch", func(t *testing.T) { dlqDeletePartialMatch(t, store) })
	t.Run("DeleteByFilterRoute", func(t *testing.T) { dlqDeleteByFilterRoute(t, store) })
	t.Run("DeleteByFilterCategory", func(t *testing.T) { dlqDeleteByFilterCategory(t, store) })
	t.Run("DeleteByFilterTimeRange", func(t *testing.T) { dlqDeleteByFilterTimeRange(t, store) })
	t.Run("DeleteByFilterWithLimit", func(t *testing.T) { dlqDeleteByFilterWithLimit(t, store) })
	t.Run("DeleteByFilterAll", func(t *testing.T) { dlqDeleteByFilterAll(t, store) })
	t.Run("PurgeRemovesOld", func(t *testing.T) { dlqPurgeRemovesOld(t, store) })
	t.Run("PurgeSkipsRecent", func(t *testing.T) { dlqPurgeSkipsRecent(t, store) })
	t.Run("FullLifecycle", func(t *testing.T) { dlqFullLifecycle(t, store) })
}

func dlqWriteAndList(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	e1 := makeDLQEntry("wal-1", "route-wal", "timeout", dlqT1)
	e2 := makeDLQEntry("wal-2", "route-wal", "rejected", dlqT2)

	if err := store.Write(ctx, e1); err != nil {
		t.Fatalf("write e1: %v", err)
	}
	if err := store.Write(ctx, e2); err != nil {
		t.Fatalf("write e2: %v", err)
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-wal"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}

	ids := dlqEntryIDs(results)
	if !ids["wal-1"] {
		t.Fatal("missing entry wal-1")
	}
	if !ids["wal-2"] {
		t.Fatal("missing entry wal-2")
	}

	var found *domain.DLQEntry
	for i := range results {
		if results[i].ID == "wal-1" {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatal("entry wal-1 not found in results")
	}

	if found.RouteID != "route-wal" {
		t.Fatalf("RouteID: got %q, want %q", found.RouteID, "route-wal")
	}
	if found.Category != "timeout" {
		t.Fatalf("Category: got %q, want %q", found.Category, "timeout")
	}
	if found.Reason != "test failure" {
		t.Fatalf("Reason: got %q, want %q", found.Reason, "test failure")
	}
	if found.ErrorCode != "TEST_ERROR" {
		t.Fatalf("ErrorCode: got %q, want %q", found.ErrorCode, "TEST_ERROR")
	}
	if found.LastError != "something went wrong" {
		t.Fatalf("LastError: got %q, want %q", found.LastError, "something went wrong")
	}
	if found.BindingID != "bind-wal-1" {
		t.Fatalf("BindingID: got %q, want %q", found.BindingID, "bind-wal-1")
	}
	if found.SessionID != "sess-wal-1" {
		t.Fatalf("SessionID: got %q, want %q", found.SessionID, "sess-wal-1")
	}
	if found.SourceID != "src-wal-1" {
		t.Fatalf("SourceID: got %q, want %q", found.SourceID, "src-wal-1")
	}
	if found.CorrelationID != "corr-wal-1" {
		t.Fatalf("CorrelationID: got %q, want %q", found.CorrelationID, "corr-wal-1")
	}
	if !found.FailedAt.Equal(dlqT1) {
		t.Fatalf("FailedAt: got %v, want %v", found.FailedAt, dlqT1)
	}
	if found.Attempts != 1 {
		t.Fatalf("Attempts: got %d, want %d", found.Attempts, 1)
	}
	if found.Envelope.ID != "env-wal-1" {
		t.Fatalf("Envelope.ID: got %q, want %q", found.Envelope.ID, "env-wal-1")
	}
	if found.Envelope.Subject != "test/subject" {
		t.Fatalf("Envelope.Subject: got %q, want %q", found.Envelope.Subject, "test/subject")
	}
	if string(found.Envelope.Payload) != `{"key":"value"}` {
		t.Fatalf("Envelope.Payload: got %q, want %q", found.Envelope.Payload, `{"key":"value"}`)
	}
	trace, ok := found.Envelope.Headers["trace"]
	if !ok || trace != "tid-wal-1" {
		t.Fatalf("Envelope.Headers[trace]: got %v, want %q", trace, "tid-wal-1")
	}
}

func dlqListFilterByRouteID(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("lfr-1", "route-alpha", "timeout", dlqT1)); err != nil {
		t.Fatalf("write lfr-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("lfr-2", "route-alpha", "timeout", dlqT2)); err != nil {
		t.Fatalf("write lfr-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("lfr-3", "route-beta", "timeout", dlqT3)); err != nil {
		t.Fatalf("write lfr-3: %v", err)
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-alpha"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}

	ids := dlqEntryIDs(results)
	if !ids["lfr-1"] {
		t.Fatal("missing entry lfr-1")
	}
	if !ids["lfr-2"] {
		t.Fatal("missing entry lfr-2")
	}
	if ids["lfr-3"] {
		t.Fatal("lfr-3 should not appear in route-alpha results")
	}
}

func dlqListFilterByCategory(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("lfc-1", "route-lfc", "timeout", dlqT1)); err != nil {
		t.Fatalf("write lfc-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("lfc-2", "route-lfc", "timeout", dlqT2)); err != nil {
		t.Fatalf("write lfc-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("lfc-3", "route-lfc", "rejected", dlqT3)); err != nil {
		t.Fatalf("write lfc-3: %v", err)
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-lfc", Category: "timeout"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}

	ids := dlqEntryIDs(results)
	if !ids["lfc-1"] {
		t.Fatal("missing entry lfc-1")
	}
	if !ids["lfc-2"] {
		t.Fatal("missing entry lfc-2")
	}
	if ids["lfc-3"] {
		t.Fatal("lfc-3 should not appear in timeout-filtered results")
	}
}

func dlqListFilterBySince(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("lfs-1", "route-lfs", "timeout", dlqT1)); err != nil {
		t.Fatalf("write lfs-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("lfs-2", "route-lfs", "timeout", dlqT2)); err != nil {
		t.Fatalf("write lfs-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("lfs-3", "route-lfs", "timeout", dlqT3)); err != nil {
		t.Fatalf("write lfs-3: %v", err)
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-lfs", Since: dlqT2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}

	ids := dlqEntryIDs(results)
	if !ids["lfs-2"] {
		t.Fatal("missing entry lfs-2")
	}
	if !ids["lfs-3"] {
		t.Fatal("missing entry lfs-3")
	}
	if ids["lfs-1"] {
		t.Fatal("lfs-1 should not appear (before Since)")
	}

	for _, e := range results {
		if e.FailedAt.Before(dlqT2) {
			t.Fatalf("entry %s has FailedAt %v which is before Since %v", e.ID, e.FailedAt, dlqT2)
		}
	}
}

func dlqListFilterByBefore(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("lfb-1", "route-lfb", "timeout", dlqT1)); err != nil {
		t.Fatalf("write lfb-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("lfb-2", "route-lfb", "timeout", dlqT2)); err != nil {
		t.Fatalf("write lfb-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("lfb-3", "route-lfb", "timeout", dlqT3)); err != nil {
		t.Fatalf("write lfb-3: %v", err)
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-lfb", Before: dlqT2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}

	ids := dlqEntryIDs(results)
	if !ids["lfb-1"] {
		t.Fatal("missing entry lfb-1")
	}
	if ids["lfb-2"] {
		t.Fatal("lfb-2 should not appear (not before Before)")
	}
	if ids["lfb-3"] {
		t.Fatal("lfb-3 should not appear (not before Before)")
	}
}

func dlqListRespectsLimit(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	times := []time.Time{dlqT1, dlqT2, dlqT3, dlqT4, dlqT5}
	for i := 0; i < 5; i++ {
		e := makeDLQEntry("lrl-"+itoa(i+1), "route-lrl", "timeout", times[i])
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write lrl-%d: %v", i+1, err)
		}
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-lrl", Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}
}

func dlqWriteIdempotent(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	e := makeDLQEntry("wi-1", "route-wi", "timeout", dlqT1)
	if err := store.Write(ctx, e); err != nil {
		t.Fatalf("first write: %v", err)
	}

	err := store.Write(ctx, e)
	if !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord, got %v", err)
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-wi"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entry (no duplicate), got %d", len(results))
	}
}

func dlqPurgeRemovesOld(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("pro-1", "route-pro", "timeout", dlqT1)); err != nil {
		t.Fatalf("write pro-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("pro-2", "route-pro", "timeout", dlqT2)); err != nil {
		t.Fatalf("write pro-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("pro-3", "route-pro", "timeout", dlqT4)); err != nil {
		t.Fatalf("write pro-3: %v", err)
	}

	n, err := store.Purge(ctx, dlqT3)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	// Purge is global (not route-scoped), so entries from prior sub-tests with
	// FailedAt < dlqT3 are also removed. Assert at least our 2 entries were purged.
	if n < 2 {
		t.Fatalf("expected at least 2 purged, got %d", n)
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-pro"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 surviving entry, got %d", len(results))
	}
	if results[0].ID != "pro-3" {
		t.Fatalf("surviving entry: got %q, want %q", results[0].ID, "pro-3")
	}
}

func dlqPurgeSkipsRecent(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("psr-1", "route-psr", "timeout", dlqT3)); err != nil {
		t.Fatalf("write psr-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("psr-2", "route-psr", "timeout", dlqT4)); err != nil {
		t.Fatalf("write psr-2: %v", err)
	}

	n, err := store.Purge(ctx, dlqT3)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 purged (all entries >= cutoff), got %d", n)
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-psr"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 surviving entries, got %d", len(results))
	}
}

func dlqFullLifecycle(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	entries := []domain.DLQEntry{
		makeDLQEntry("flc-1", "route-flc", "timeout", dlqT1),
		makeDLQEntry("flc-2", "route-flc", "rejected", dlqT2),
		makeDLQEntry("flc-3", "route-flc", "timeout", dlqT3),
		makeDLQEntry("flc-4", "route-flc", "rejected", dlqT4),
	}
	for _, e := range entries {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %s: %v", e.ID, err)
		}
	}

	results, err := store.List(ctx, domain.DLQFilter{RouteID: "route-flc"})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(results))
	}

	// Get single entry
	got, err := store.Get(ctx, "flc-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "flc-1" {
		t.Fatalf("get ID: got %q, want %q", got.ID, "flc-1")
	}

	// Delete specific entry
	n, err := store.Delete(ctx, []string{"flc-1"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted, got %d", n)
	}

	// Purge old entries
	pn, err := store.Purge(ctx, dlqT3)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if pn < 1 {
		t.Fatalf("expected at least 1 purged, got %d", pn)
	}

	results, err = store.List(ctx, domain.DLQFilter{RouteID: "route-flc"})
	if err != nil {
		t.Fatalf("list after purge: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(results))
	}

	ids := dlqEntryIDs(results)
	if !ids["flc-3"] {
		t.Fatal("missing entry flc-3")
	}
	if !ids["flc-4"] {
		t.Fatal("missing entry flc-4")
	}
}

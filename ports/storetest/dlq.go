package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
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

func makeDLQEntry(id, routeID, category string, failedAt time.Time) routing.DLQEntry {
	return routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:            id,
		RouteID:       routeID,
		BindingID:     "bind-" + id,
		SessionID:     "sess-" + id,
		SourceID:      "src-" + id,
		CorrelationID: "corr-" + id,
		Address:       "addr/" + id,
		Reason:        "test failure",
		Category:      category,
		ErrorCode:     "TEST_ERROR",
		LastError:     "something went wrong",
		FailedAt:      failedAt,
		Attempts:      1,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-" + id,
			Subject: "test/subject",
			Payload: []byte(`{"key":"value"}`),
			Headers: map[string]any{"trace": "tid-" + id},
		}),
	})
}

func dlqEntryIDs(entries []routing.DLQEntry) map[string]bool {
	m := make(map[string]bool, len(entries))
	for _, e := range entries {
		m[e.ID()] = true
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
	t.Run("ListOldestFirst", func(t *testing.T) { dlqListOldestFirst(t, store) })
	t.Run("WriteIdempotent", func(t *testing.T) { dlqWriteIdempotent(t, store) })
	t.Run("GetExisting", func(t *testing.T) { dlqGetExisting(t, store) })
	t.Run("EnvelopeIsolation", func(t *testing.T) { dlqEnvelopeIsolation(t, store) })
	t.Run("GetMissing", func(t *testing.T) { dlqGetMissing(t, store) })
	t.Run("DeleteByIDs", func(t *testing.T) { dlqDeleteByIDs(t, store) })
	t.Run("DeleteMissing", func(t *testing.T) { dlqDeleteMissing(t, store) })
	t.Run("DeletePartialMatch", func(t *testing.T) { dlqDeletePartialMatch(t, store) })
	t.Run("DeleteByFilterRoute", func(t *testing.T) { dlqDeleteByFilterRoute(t, store) })
	t.Run("DeleteByFilterCategory", func(t *testing.T) { dlqDeleteByFilterCategory(t, store) })
	t.Run("DeleteByFilterTimeRange", func(t *testing.T) { dlqDeleteByFilterTimeRange(t, store) })
	t.Run("DeleteByFilterWithLimit", func(t *testing.T) { dlqDeleteByFilterWithLimit(t, store) })
	t.Run("DeleteByFilterAll", func(t *testing.T) { dlqDeleteByFilterAll(t, store) })
	t.Run("DeleteByFilterExhaustsScanPages", func(t *testing.T) { dlqDeleteByFilterExhaustsScanPages(t, store) })
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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-wal"})
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

	var found *routing.DLQEntry
	for i := range results {
		if results[i].ID() == "wal-1" {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatal("entry wal-1 not found in results")
	}

	if found.RouteID() != "route-wal" {
		t.Fatalf("RouteID: got %q, want %q", found.RouteID(), "route-wal")
	}
	if found.Category() != "timeout" {
		t.Fatalf("Category: got %q, want %q", found.Category(), "timeout")
	}
	if found.Reason() != "test failure" {
		t.Fatalf("Reason: got %q, want %q", found.Reason(), "test failure")
	}
	if found.ErrorCode() != "TEST_ERROR" {
		t.Fatalf("ErrorCode: got %q, want %q", found.ErrorCode(), "TEST_ERROR")
	}
	if found.LastError() != "something went wrong" {
		t.Fatalf("LastError: got %q, want %q", found.LastError(), "something went wrong")
	}
	if found.BindingID() != "bind-wal-1" {
		t.Fatalf("BindingID: got %q, want %q", found.BindingID(), "bind-wal-1")
	}
	if found.SessionID() != "sess-wal-1" {
		t.Fatalf("SessionID: got %q, want %q", found.SessionID(), "sess-wal-1")
	}
	if found.SourceID() != "src-wal-1" {
		t.Fatalf("SourceID: got %q, want %q", found.SourceID(), "src-wal-1")
	}
	if found.CorrelationID() != "corr-wal-1" {
		t.Fatalf("CorrelationID: got %q, want %q", found.CorrelationID(), "corr-wal-1")
	}
	if found.Address() != "addr/wal-1" {
		t.Fatalf("Address: got %q, want %q", found.Address(), "addr/wal-1")
	}
	if !found.FailedAt().Equal(dlqT1) {
		t.Fatalf("FailedAt: got %v, want %v", found.FailedAt(), dlqT1)
	}
	if found.Attempts() != 1 {
		t.Fatalf("Attempts: got %d, want %d", found.Attempts(), 1)
	}
	env := found.Snapshot()
	if env.ID() != "env-wal-1" {
		t.Fatalf("Envelope.ID: got %q, want %q", env.ID(), "env-wal-1")
	}
	if env.Subject() != "test/subject" {
		t.Fatalf("Envelope.Subject: got %q, want %q", env.Subject(), "test/subject")
	}
	if string(env.Payload()) != `{"key":"value"}` {
		t.Fatalf("Envelope.Payload: got %q, want %q", env.Payload(), `{"key":"value"}`)
	}
	trace, ok := env.Headers()["trace"]
	if !ok || trace != "tid-wal-1" {
		t.Fatalf("Envelope.Headers[trace]: got %v, want %q", trace, "tid-wal-1")
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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-wi"})
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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-pro"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 surviving entry, got %d", len(results))
	}
	if results[0].ID() != "pro-3" {
		t.Fatalf("surviving entry: got %q, want %q", results[0].ID(), "pro-3")
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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-psr"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 surviving entries, got %d", len(results))
	}
}

func dlqFullLifecycle(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	entries := []routing.DLQEntry{
		makeDLQEntry("flc-1", "route-flc", "timeout", dlqT1),
		makeDLQEntry("flc-2", "route-flc", "rejected", dlqT2),
		makeDLQEntry("flc-3", "route-flc", "timeout", dlqT3),
		makeDLQEntry("flc-4", "route-flc", "rejected", dlqT4),
	}
	for _, e := range entries {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %s: %v", e.ID(), err)
		}
	}

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-flc"})
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
	if got.ID() != "flc-1" {
		t.Fatalf("get ID: got %q, want %q", got.ID(), "flc-1")
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

	results, err = store.List(ctx, routing.DLQFilter{RouteID: "route-flc"})
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

// dlqEnvelopeIsolation proves the DLQStore clone boundary:
// the persisted envelope is independent of both the caller's input
// reference and any returned snapshot, so no caller-side mutation can
// corrupt stored state.
//
// The entry is built with RehydrateDLQEntry, the non-cloning constructor,
// so its envelope deliberately aliases the caller-held envelope's headers
// map — exactly the hand-off a storage adapter must defend against at its
// Write boundary.
func dlqEnvelopeIsolation(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()
	const wantTrace = "trace-iso"

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-iso",
		Subject: "iso/subject",
		Payload: []byte(`{"k":"v"}`),
		Headers: map[string]any{"trace": wantTrace},
	})
	entry := routing.RehydrateDLQEntry(routing.DLQEntrySpec{
		ID:       "iso-1",
		RouteID:  "route-iso",
		Envelope: *env,
		FailedAt: dlqT1,
	})
	if err := store.Write(ctx, entry); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Mutate the caller-held envelope after Write. A store that cloned at
	// its Write boundary is immune; one that retained the reference leaks.
	env.SetHeader("trace", "tampered-in")
	env.SetHeader("added", "late")

	got, err := store.Get(ctx, "iso-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	gotEnv := got.Snapshot()
	if v, _ := gotEnv.Header("trace"); v != wantTrace {
		t.Fatalf("stored header mutated via caller reference: got %v, want %q", v, wantTrace)
	}
	if _, ok := gotEnv.Header("added"); ok {
		t.Fatal("late-added caller header leaked into stored entry")
	}

	// Mutating a Get result's snapshot must not change a subsequent read:
	// the store must hand out envelopes independent of its stored state.
	gotEnv.SetHeader("trace", "tampered-out")
	again, err := store.Get(ctx, "iso-1")
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if v, _ := again.Snapshot().Header("trace"); v != wantTrace {
		t.Fatalf("Get snapshot mutation leaked into store: got %v, want %q", v, wantTrace)
	}

	// List must likewise return entries independent of stored state.
	list, err := store.List(ctx, routing.DLQFilter{RouteID: "route-iso"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for i := range list {
		if list[i].ID() != "iso-1" {
			continue
		}
		found = true
		if v, _ := list[i].Snapshot().Header("trace"); v != wantTrace {
			t.Fatalf("List entry header not isolated: got %v, want %q", v, wantTrace)
		}
	}
	if !found {
		t.Fatal("entry iso-1 not returned by List")
	}
}

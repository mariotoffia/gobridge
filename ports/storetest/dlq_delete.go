package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func dlqGetExisting(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	e := makeDLQEntry("ge-1", "route-ge", "timeout", dlqT1)
	if err := store.Write(ctx, e); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := store.Get(ctx, "ge-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "ge-1" {
		t.Fatalf("ID: got %q, want %q", got.ID, "ge-1")
	}
	if got.RouteID != "route-ge" {
		t.Fatalf("RouteID: got %q, want %q", got.RouteID, "route-ge")
	}
	if got.Category != "timeout" {
		t.Fatalf("Category: got %q, want %q", got.Category, "timeout")
	}
	if got.Envelope.ID != "env-ge-1" {
		t.Fatalf("Envelope.ID: got %q, want %q", got.Envelope.ID, "env-ge-1")
	}
	if string(got.Envelope.Payload) != `{"key":"value"}` {
		t.Fatalf("Envelope.Payload: got %q, want %q", got.Envelope.Payload, `{"key":"value"}`)
	}
}

func dlqGetMissing(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexistent-id")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func dlqDeleteByIDs(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("del-1", "route-del", "timeout", dlqT1)); err != nil {
		t.Fatalf("write del-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("del-2", "route-del", "timeout", dlqT2)); err != nil {
		t.Fatalf("write del-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("del-3", "route-del", "timeout", dlqT3)); err != nil {
		t.Fatalf("write del-3: %v", err)
	}

	n, err := store.Delete(ctx, []string{"del-1", "del-2"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-del"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(results))
	}
	if results[0].ID != "del-3" {
		t.Fatalf("remaining: got %q, want %q", results[0].ID, "del-3")
	}
}

func dlqDeleteMissing(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	n, err := store.Delete(ctx, []string{"no-such-id-1", "no-such-id-2"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 deleted for missing IDs, got %d", n)
	}
}

func dlqDeletePartialMatch(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("dpm-1", "route-dpm", "timeout", dlqT1)); err != nil {
		t.Fatalf("write dpm-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dpm-2", "route-dpm", "timeout", dlqT2)); err != nil {
		t.Fatalf("write dpm-2: %v", err)
	}

	n, err := store.Delete(ctx, []string{"dpm-1", "dpm-nonexistent", "dpm-2"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted (skipping nonexistent), got %d", n)
	}
}

func dlqDeleteByFilterRoute(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("dfr-1", "route-dfr-a", "timeout", dlqT1)); err != nil {
		t.Fatalf("write dfr-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dfr-2", "route-dfr-a", "timeout", dlqT2)); err != nil {
		t.Fatalf("write dfr-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dfr-3", "route-dfr-b", "timeout", dlqT3)); err != nil {
		t.Fatalf("write dfr-3: %v", err)
	}

	n, err := store.DeleteByFilter(ctx, routing.DLQFilter{RouteID: "route-dfr-a"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-dfr-b"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(results))
	}
	if results[0].ID != "dfr-3" {
		t.Fatalf("remaining: got %q, want %q", results[0].ID, "dfr-3")
	}
}

func dlqDeleteByFilterCategory(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("dfc-1", "route-dfc", "timeout", dlqT1)); err != nil {
		t.Fatalf("write dfc-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dfc-2", "route-dfc", "rejected", dlqT2)); err != nil {
		t.Fatalf("write dfc-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dfc-3", "route-dfc", "timeout", dlqT3)); err != nil {
		t.Fatalf("write dfc-3: %v", err)
	}

	n, err := store.DeleteByFilter(ctx, routing.DLQFilter{RouteID: "route-dfc", Category: "timeout"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-dfc"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(results))
	}
	if results[0].ID != "dfc-2" {
		t.Fatalf("remaining: got %q, want %q", results[0].ID, "dfc-2")
	}
}

func dlqDeleteByFilterTimeRange(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("dft-1", "route-dft", "timeout", dlqT1)); err != nil {
		t.Fatalf("write dft-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dft-2", "route-dft", "timeout", dlqT2)); err != nil {
		t.Fatalf("write dft-2: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dft-3", "route-dft", "timeout", dlqT3)); err != nil {
		t.Fatalf("write dft-3: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dft-4", "route-dft", "timeout", dlqT4)); err != nil {
		t.Fatalf("write dft-4: %v", err)
	}

	// Delete entries in the [dlqT2, dlqT4) range — should remove dft-2 and dft-3
	n, err := store.DeleteByFilter(ctx, routing.DLQFilter{
		RouteID: "route-dft",
		Since:   dlqT2,
		Before:  dlqT4,
	})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-dft"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(results))
	}

	ids := dlqEntryIDs(results)
	if !ids["dft-1"] {
		t.Fatal("missing entry dft-1")
	}
	if !ids["dft-4"] {
		t.Fatal("missing entry dft-4")
	}
}

func dlqDeleteByFilterWithLimit(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		e := makeDLQEntry("dfl-"+itoa(i+1), "route-dfl", "timeout",
			dlqT1.Add(time.Duration(i)*time.Hour))
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write dfl-%d: %v", i+1, err)
		}
	}

	n, err := store.DeleteByFilter(ctx, routing.DLQFilter{
		RouteID: "route-dfl",
		Limit:   2,
	})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted (limited), got %d", n)
	}

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-dfl"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 remaining, got %d", len(results))
	}
}

func dlqDeleteByFilterAll(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()

	if err := store.Write(ctx, makeDLQEntry("dfa-1", "route-dfa", "timeout", dlqT1)); err != nil {
		t.Fatalf("write dfa-1: %v", err)
	}
	if err := store.Write(ctx, makeDLQEntry("dfa-2", "route-dfa", "rejected", dlqT2)); err != nil {
		t.Fatalf("write dfa-2: %v", err)
	}

	n, err := store.DeleteByFilter(ctx, routing.DLQFilter{RouteID: "route-dfa"})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-dfa"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 remaining, got %d", len(results))
	}
}

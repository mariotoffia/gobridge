package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-alpha"})
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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-lfc", Category: "timeout"})
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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-lfs", Since: dlqT2})
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
		if e.FailedAt().Before(dlqT2) {
			t.Fatalf("entry %s has FailedAt %v which is before Since %v", e.ID(), e.FailedAt(), dlqT2)
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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-lfb", Before: dlqT2})
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

	results, err := store.List(ctx, routing.DLQFilter{RouteID: "route-lrl", Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}
}

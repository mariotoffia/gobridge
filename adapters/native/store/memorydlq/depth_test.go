package memorydlq_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
)

// TestDepth_ReportsOutstandingCount proves Depth returns the current number of
// outstanding DLQ entries and tracks writes and deletes. A fresh store reports
// 0, each write raises the count, and a delete lowers it. Mutation-verify:
// change Depth to return a constant (e.g. len(nil)) and this test fails.
func TestDepth_ReportsOutstandingCount(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	if got, err := s.Depth(ctx); err != nil || got != 0 {
		t.Fatalf("empty Depth = %d, err=%v, want 0", got, err)
	}

	const n = 250
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("dep-%d", i)
		ids = append(ids, id)
		if err := s.Write(ctx, makeEntry(id, "route-A", "timeout", base.Add(time.Duration(i)*time.Millisecond))); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}

	if got, err := s.Depth(ctx); err != nil || got != n {
		t.Fatalf("Depth after %d writes = %d, err=%v, want %d", n, got, err, n)
	}

	// Deleting a subset lowers the standing backlog by exactly that many.
	const del = 40
	removed, err := s.Delete(ctx, ids[:del])
	if err != nil || removed != del {
		t.Fatalf("delete: removed=%d, err=%v, want %d", removed, err, del)
	}
	if got, err := s.Depth(ctx); err != nil || got != n-del {
		t.Fatalf("Depth after delete = %d, err=%v, want %d", got, err, n-del)
	}
}

package memorydlq_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

// Validates the in-memory DLQ store against the shared conformance suite.
func TestDLQStoreConformance(t *testing.T) {
	s := memorydlq.NewStore()
	storetest.RunDLQStoreTests(t, s)
}

func makeEntry(id, routeID, category string, failedAt time.Time) domain.DLQEntry {
	return domain.DLQEntry{
		ID:       id,
		RouteID:  routeID,
		Category: category,
		FailedAt: failedAt,
		Envelope: domain.Envelope{
			ID:      "env-" + id,
			Subject: "test/subject",
			Payload: []byte("payload-" + id),
		},
		BindingID:     "binding-1",
		SessionID:     "session-1",
		SourceID:      "source-1",
		CorrelationID: "corr-" + id,
		Reason:        "test failure",
		ErrorCode:     "TEST_ERROR",
		LastError:     "something went wrong",
		Attempts:      3,
	}
}

// Verifies Write persists an entry and List returns matching fields.
func TestWriteAndList(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	now := time.Now()
	entry := makeEntry("e1", "route-A", "timeout", now)

	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	entries, err := s.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("unexpected list error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	got := entries[0]
	if got.ID != "e1" {
		t.Errorf("ID: got %q, want %q", got.ID, "e1")
	}
	if got.RouteID != "route-A" {
		t.Errorf("RouteID: got %q, want %q", got.RouteID, "route-A")
	}
	if got.Category != "timeout" {
		t.Errorf("Category: got %q, want %q", got.Category, "timeout")
	}
	if !got.FailedAt.Equal(now) {
		t.Errorf("FailedAt: got %v, want %v", got.FailedAt, now)
	}
	if got.Envelope.ID != "env-e1" {
		t.Errorf("Envelope.ID: got %q, want %q", got.Envelope.ID, "env-e1")
	}
	if got.Attempts != 3 {
		t.Errorf("Attempts: got %d, want %d", got.Attempts, 3)
	}
}

// Verifies List filters entries by route ID.
func TestListFilterByRouteID(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	s.Write(ctx, makeEntry("e1", "route-A", "timeout", now))
	s.Write(ctx, makeEntry("e2", "route-B", "timeout", now))
	s.Write(ctx, makeEntry("e3", "route-A", "timeout", now))

	entries, err := s.List(ctx, domain.DLQFilter{RouteID: "route-A"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	for _, e := range entries {
		if e.RouteID != "route-A" {
			t.Errorf("unexpected RouteID %q in filtered result", e.RouteID)
		}
	}
}

// Verifies List filters entries by category.
func TestListFilterByCategory(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	s.Write(ctx, makeEntry("e1", "route-A", "timeout", now))
	s.Write(ctx, makeEntry("e2", "route-A", "schema", now))
	s.Write(ctx, makeEntry("e3", "route-A", "timeout", now))

	entries, err := s.List(ctx, domain.DLQFilter{Category: "schema"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ID != "e2" {
		t.Errorf("expected entry e2, got %q", entries[0].ID)
	}
}

// Verifies List includes only entries with FailedAt on or after Since.
func TestListFilterBySince(t *testing.T) {
	now := time.Now()
	clock := &atomic.Value{}
	clock.Store(now)

	s := memorydlq.NewStore(memorydlq.WithClock(func() time.Time {
		return clock.Load().(time.Time)
	}))
	ctx := context.Background()

	t1 := now
	t2 := now.Add(1 * time.Hour)
	t3 := now.Add(2 * time.Hour)

	s.Write(ctx, makeEntry("e1", "route-A", "timeout", t1))
	s.Write(ctx, makeEntry("e2", "route-A", "timeout", t2))
	s.Write(ctx, makeEntry("e3", "route-A", "timeout", t3))

	entries, err := s.List(ctx, domain.DLQFilter{Since: t2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (at or after midpoint), got %d", len(entries))
	}
	for _, e := range entries {
		if e.FailedAt.Before(t2) {
			t.Errorf("entry %q has FailedAt %v, which is before Since %v", e.ID, e.FailedAt, t2)
		}
	}
}

// Verifies List includes only entries with FailedAt strictly before Before.
func TestListFilterByBefore(t *testing.T) {
	now := time.Now()
	clock := &atomic.Value{}
	clock.Store(now)

	s := memorydlq.NewStore(memorydlq.WithClock(func() time.Time {
		return clock.Load().(time.Time)
	}))
	ctx := context.Background()

	t1 := now
	t2 := now.Add(1 * time.Hour)
	t3 := now.Add(2 * time.Hour)

	s.Write(ctx, makeEntry("e1", "route-A", "timeout", t1))
	s.Write(ctx, makeEntry("e2", "route-A", "timeout", t2))
	s.Write(ctx, makeEntry("e3", "route-A", "timeout", t3))

	entries, err := s.List(ctx, domain.DLQFilter{Before: t2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (strictly before midpoint), got %d", len(entries))
	}
	if entries[0].ID != "e1" {
		t.Errorf("expected entry e1, got %q", entries[0].ID)
	}
}

// Verifies List caps the number of returned entries when Limit is set.
func TestListRespectsLimit(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < 5; i++ {
		s.Write(ctx, makeEntry(fmt.Sprintf("e%d", i), "route-A", "timeout", now.Add(time.Duration(i)*time.Minute)))
	}

	entries, err := s.List(ctx, domain.DLQFilter{Limit: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

// Verifies List orders entries by FailedAt descending.
func TestListSortedByFailedAtDescending(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	s.Write(ctx, makeEntry("e-old", "route-A", "timeout", now))
	s.Write(ctx, makeEntry("e-mid", "route-A", "timeout", now.Add(1*time.Hour)))
	s.Write(ctx, makeEntry("e-new", "route-A", "timeout", now.Add(2*time.Hour)))

	entries, err := s.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].ID != "e-new" {
		t.Errorf("first entry: got %q, want %q", entries[0].ID, "e-new")
	}
	if entries[1].ID != "e-mid" {
		t.Errorf("second entry: got %q, want %q", entries[1].ID, "e-mid")
	}
	if entries[2].ID != "e-old" {
		t.Errorf("third entry: got %q, want %q", entries[2].ID, "e-old")
	}
}

// Verifies duplicate writes for the same entry ID return ErrDuplicateRecord.
func TestWriteIdempotent(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	entry := makeEntry("e1", "route-A", "timeout", now)

	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("first write: %v", err)
	}

	err := s.Write(ctx, entry)
	if !errors.Is(err, domain.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord, got %v", err)
	}
}

// Verifies Replay succeeds for existing entry IDs.
func TestReplayMarksEntries(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	s.Write(ctx, makeEntry("e1", "route-A", "timeout", now))
	s.Write(ctx, makeEntry("e2", "route-A", "timeout", now))

	if err := s.Replay(ctx, []string{"e1", "e2"}); err != nil {
		t.Fatalf("unexpected replay error: %v", err)
	}
}

// Verifies Replay returns ErrNotFound when no matching entries exist.
func TestReplayNotFound(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	err := s.Replay(ctx, []string{"nonexistent"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Verifies Replay returns ErrNotFound when any requested ID is missing.
func TestReplayPartialNotFound(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	s.Write(ctx, makeEntry("e1", "route-A", "timeout", now))

	err := s.Replay(ctx, []string{"e1", "nonexistent"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Verifies Purge deletes entries older than the cutoff and reports the count removed.
func TestPurgeRemovesOld(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	s.Write(ctx, makeEntry("e-old", "route-A", "timeout", now.Add(-2*time.Hour)))
	s.Write(ctx, makeEntry("e-mid", "route-A", "timeout", now.Add(-1*time.Hour)))
	s.Write(ctx, makeEntry("e-new", "route-A", "timeout", now))

	cutoff := now.Add(-30 * time.Minute)
	n, err := s.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("unexpected purge error: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged count: got %d, want 2", n)
	}

	entries, err := s.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("unexpected list error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 remaining entry, got %d", len(entries))
	}
	if entries[0].ID != "e-new" {
		t.Errorf("remaining entry: got %q, want %q", entries[0].ID, "e-new")
	}
}

// Verifies Purge only removes entries strictly older than the cutoff.
func TestPurgeSkipsRecent(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	s.Write(ctx, makeEntry("e1", "route-A", "timeout", now.Add(-1*time.Hour)))
	s.Write(ctx, makeEntry("e2", "route-A", "timeout", now))
	s.Write(ctx, makeEntry("e3", "route-A", "timeout", now.Add(1*time.Hour)))

	cutoff := now.Add(-30 * time.Minute)
	n, err := s.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("unexpected purge error: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged count: got %d, want 1", n)
	}

	entries, err := s.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("unexpected list error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 remaining entries, got %d", len(entries))
	}
}

// Verifies Purge on an empty store returns zero without error.
func TestPurgeReturnsZeroWhenEmpty(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	n, err := s.Purge(ctx, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("purged count: got %d, want 0", n)
	}
}

// Demonstrates list, replay, and purge behavior across multiple entries and filters.
func TestFullLifecycle(t *testing.T) {
	now := time.Now()
	clock := &atomic.Value{}
	clock.Store(now)

	s := memorydlq.NewStore(memorydlq.WithClock(func() time.Time {
		return clock.Load().(time.Time)
	}))
	ctx := context.Background()

	s.Write(ctx, makeEntry("e1", "route-A", "timeout", now.Add(-3*time.Hour)))
	s.Write(ctx, makeEntry("e2", "route-A", "schema", now.Add(-2*time.Hour)))
	s.Write(ctx, makeEntry("e3", "route-B", "timeout", now.Add(-1*time.Hour)))
	s.Write(ctx, makeEntry("e4", "route-B", "auth", now))

	entries, err := s.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("initial list: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	if err := s.Replay(ctx, []string{"e1", "e3"}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	cutoff := now.Add(-90 * time.Minute)
	n, err := s.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged count: got %d, want 2", n)
	}

	entries, err = s.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("final list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 remaining entries, got %d", len(entries))
	}

	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.ID] = true
	}
	if !ids["e3"] {
		t.Error("expected e3 to remain")
	}
	if !ids["e4"] {
		t.Error("expected e4 to remain")
	}
}

// Verifies concurrent writes produce distinct entries without data races.
func TestConcurrentWrite(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()
	now := time.Now()

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("e%d", idx)
			if err := s.Write(ctx, makeEntry(id, "route-A", "timeout", now)); err != nil {
				t.Errorf("write goroutine %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	entries, err := s.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("unexpected list error: %v", err)
	}
	if len(entries) != goroutines {
		t.Fatalf("expected %d entries, got %d", goroutines, len(entries))
	}
}

// Verifies List on an empty store returns an empty result.
func TestListEmptyStore(t *testing.T) {
	s := memorydlq.NewStore()
	ctx := context.Background()

	entries, err := s.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries != nil && len(entries) != 0 {
		t.Fatalf("expected empty slice, got %d entries", len(entries))
	}
}

package sqlitedlq_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

// Validates the SQLite DLQ store against the shared conformance suite.
func TestDLQStoreConformance(t *testing.T) {
	s := newTempStore(t)
	storetest.RunDLQStoreTests(t, s)
}

func newTempStore(t *testing.T) *sqlitedlq.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dlq.db")
	s, err := sqlitedlq.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustWrite(t *testing.T, s *sqlitedlq.Store, ctx context.Context, entry routing.DLQEntry) {
	t.Helper()
	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("Write %s: %v", entry.ID, err)
	}
}

func makeEntry(id, routeID, category string, failedAt time.Time) routing.DLQEntry {
	return routing.DLQEntry{
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
		Attempts:      3,
		Envelope: messaging.Envelope{
			ID:      "env-" + id,
			Subject: "test/subject",
			Payload: []byte(`{"key":"value"}`),
			Headers: map[string]any{"x-test": "header"},
		},
	}
}

// Verifies Write persists a full entry and List returns matching fields.
func TestWriteAndList(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	entry := makeEntry("e1", "route-A", "timeout", now)

	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}

	g := got[0]
	if g.ID != "e1" {
		t.Fatalf("ID: got %q, want %q", g.ID, "e1")
	}
	if g.RouteID != "route-A" {
		t.Fatalf("RouteID: got %q, want %q", g.RouteID, "route-A")
	}
	if g.BindingID != "bind-e1" {
		t.Fatalf("BindingID: got %q, want %q", g.BindingID, "bind-e1")
	}
	if g.SessionID != "sess-e1" {
		t.Fatalf("SessionID: got %q, want %q", g.SessionID, "sess-e1")
	}
	if g.SourceID != "src-e1" {
		t.Fatalf("SourceID: got %q, want %q", g.SourceID, "src-e1")
	}
	if g.CorrelationID != "corr-e1" {
		t.Fatalf("CorrelationID: got %q, want %q", g.CorrelationID, "corr-e1")
	}
	if g.Reason != "test failure" {
		t.Fatalf("Reason: got %q, want %q", g.Reason, "test failure")
	}
	if g.Category != "timeout" {
		t.Fatalf("Category: got %q, want %q", g.Category, "timeout")
	}
	if g.ErrorCode != "TEST_ERROR" {
		t.Fatalf("ErrorCode: got %q, want %q", g.ErrorCode, "TEST_ERROR")
	}
	if g.LastError != "something went wrong" {
		t.Fatalf("LastError: got %q, want %q", g.LastError, "something went wrong")
	}
	if !g.FailedAt.Equal(now) {
		t.Fatalf("FailedAt: got %v, want %v", g.FailedAt, now)
	}
	if g.Attempts != 3 {
		t.Fatalf("Attempts: got %d, want 3", g.Attempts)
	}
	if g.Envelope.ID != "env-e1" {
		t.Fatalf("Envelope.ID: got %q, want %q", g.Envelope.ID, "env-e1")
	}
	if g.Envelope.Subject != "test/subject" {
		t.Fatalf("Envelope.Subject: got %q, want %q", g.Envelope.Subject, "test/subject")
	}
	if string(g.Envelope.Payload) != `{"key":"value"}` {
		t.Fatalf("Envelope.Payload: got %q", g.Envelope.Payload)
	}
	if v, ok := g.Envelope.Headers["x-test"]; !ok || v != "header" {
		t.Fatalf("Envelope.Headers: got %v", g.Envelope.Headers)
	}
}

// Verifies List filters entries by route ID.
func TestListFilterByRouteID(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	if err := s.Write(ctx, makeEntry("r1", "route-A", "cat", now)); err != nil {
		t.Fatalf("Write r1: %v", err)
	}
	if err := s.Write(ctx, makeEntry("r2", "route-B", "cat", now)); err != nil {
		t.Fatalf("Write r2: %v", err)
	}
	if err := s.Write(ctx, makeEntry("r3", "route-A", "cat", now)); err != nil {
		t.Fatalf("Write r3: %v", err)
	}

	got, err := s.List(ctx, routing.DLQFilter{RouteID: "route-A"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries for route-A, got %d", len(got))
	}
	for _, e := range got {
		if e.RouteID != "route-A" {
			t.Fatalf("unexpected RouteID %q in filtered results", e.RouteID)
		}
	}
}

// Verifies List filters entries by category.
func TestListFilterByCategory(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	if err := s.Write(ctx, makeEntry("c1", "route-A", "timeout", now)); err != nil {
		t.Fatalf("Write c1: %v", err)
	}
	if err := s.Write(ctx, makeEntry("c2", "route-A", "rejected", now)); err != nil {
		t.Fatalf("Write c2: %v", err)
	}
	if err := s.Write(ctx, makeEntry("c3", "route-A", "timeout", now)); err != nil {
		t.Fatalf("Write c3: %v", err)
	}

	got, err := s.List(ctx, routing.DLQFilter{Category: "timeout"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries for category timeout, got %d", len(got))
	}
	for _, e := range got {
		if e.Category != "timeout" {
			t.Fatalf("unexpected Category %q in filtered results", e.Category)
		}
	}
}

// Verifies List respects the Since lower bound on FailedAt.
func TestListFilterBySince(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	t1h := now.Add(-1 * time.Hour)
	t30m := now.Add(-30 * time.Minute)

	mustWrite(t, s, ctx, makeEntry("s1", "route-A", "cat", t1h))
	mustWrite(t, s, ctx, makeEntry("s2", "route-A", "cat", t30m))
	mustWrite(t, s, ctx, makeEntry("s3", "route-A", "cat", now))

	since := now.Add(-45 * time.Minute)
	got, err := s.List(ctx, routing.DLQFilter{Since: since})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries since 45min ago, got %d", len(got))
	}
	for _, e := range got {
		if e.FailedAt.Before(since) {
			t.Fatalf("entry %q FailedAt %v is before Since %v", e.ID, e.FailedAt, since)
		}
	}
}

// Verifies List respects the Before upper bound on FailedAt.
func TestListFilterByBefore(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	t1h := now.Add(-1 * time.Hour)
	t30m := now.Add(-30 * time.Minute)

	mustWrite(t, s, ctx, makeEntry("b1", "route-A", "cat", t1h))
	mustWrite(t, s, ctx, makeEntry("b2", "route-A", "cat", t30m))
	mustWrite(t, s, ctx, makeEntry("b3", "route-A", "cat", now))

	before := now.Add(-20 * time.Minute)
	got, err := s.List(ctx, routing.DLQFilter{Before: before})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries before 20min ago, got %d", len(got))
	}
	for _, e := range got {
		if !e.FailedAt.Before(before) {
			t.Fatalf("entry %q FailedAt %v is not before %v", e.ID, e.FailedAt, before)
		}
	}
}

// Verifies List caps results when Limit is set.
func TestListRespectsLimit(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		id := "lim-" + string(rune('a'+i))
		mustWrite(t, s, ctx, makeEntry(id, "route-A", "cat", now.Add(time.Duration(i)*time.Second)))
	}

	got, err := s.List(ctx, routing.DLQFilter{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries with Limit=2, got %d", len(got))
	}
}

// Verifies duplicate writes return ErrDuplicateRecord and do not create extra rows.
func TestWriteIdempotent(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	entry := makeEntry("dup-1", "route-A", "cat", now)

	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	err := s.Write(ctx, entry)
	if err == nil {
		t.Fatal("expected error on duplicate Write, got nil")
	}
	if !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord, got %v", err)
	}

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry after duplicate write, got %d", len(got))
	}
}

// Verifies Delete removes entries and returns the correct count.
func TestDeleteRemovesEntries(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	mustWrite(t, s, ctx, makeEntry("rp1", "route-A", "cat", now))
	mustWrite(t, s, ctx, makeEntry("rp2", "route-A", "cat", now))

	count, err := s.Delete(ctx, []string{"rp1"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if count != 1 {
		t.Fatalf("Delete count: got %d, want 1", count)
	}

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry after delete, got %d", len(got))
	}
	if got[0].ID != "rp2" {
		t.Fatalf("remaining entry ID: got %q, want %q", got[0].ID, "rp2")
	}

	// Deleting the same ID again silently returns count=0, no error.
	count, err = s.Delete(ctx, []string{"rp1"})
	if err != nil {
		t.Fatalf("second Delete: unexpected error %v", err)
	}
	if count != 0 {
		t.Fatalf("second Delete count: got %d, want 0", count)
	}
}

// Verifies Purge deletes older entries and leaves newer ones.
func TestPurgeRemovesOld(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	t2h := now.Add(-2 * time.Hour)
	t1h := now.Add(-1 * time.Hour)

	mustWrite(t, s, ctx, makeEntry("p1", "route-A", "cat", t2h))
	mustWrite(t, s, ctx, makeEntry("p2", "route-A", "cat", t1h))
	mustWrite(t, s, ctx, makeEntry("p3", "route-A", "cat", now))

	cutoff := now.Add(-30 * time.Minute)
	count, err := s.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if count != 2 {
		t.Fatalf("Purge count: got %d, want 2", count)
	}

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 remaining entry after purge, got %d", len(got))
	}
	if got[0].ID != "p3" {
		t.Fatalf("remaining entry ID: got %q, want %q", got[0].ID, "p3")
	}
}

// Verifies Purge does not remove entries newer than the cutoff.
func TestPurgeSkipsRecent(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	mustWrite(t, s, ctx, makeEntry("recent-1", "route-A", "cat", now))

	cutoff := now.Add(-1 * time.Hour)
	count, err := s.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if count != 0 {
		t.Fatalf("Purge count: got %d, want 0", count)
	}

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry still present, got %d", len(got))
	}
}

// Demonstrates write, list, delete, and purge clearing persisted rows.
func TestFullLifecycle(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	past := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	mustWrite(t, s, ctx, makeEntry("lc1", "route-A", "timeout", past))
	mustWrite(t, s, ctx, makeEntry("lc2", "route-A", "timeout", past))

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List after write: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries after write, got %d", len(got))
	}

	count, err := s.Delete(ctx, []string{"lc1"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if count != 1 {
		t.Fatalf("Delete count: got %d, want 1", count)
	}

	// Deleting the same ID again returns count=0, no error.
	count, err = s.Delete(ctx, []string{"lc1"})
	if err != nil {
		t.Fatalf("second Delete: unexpected error %v", err)
	}
	if count != 0 {
		t.Fatalf("second Delete count: got %d, want 0", count)
	}

	cutoff := time.Now().Add(-1 * time.Hour)
	count, err = s.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if count != 1 {
		t.Fatalf("Purge count: got %d, want 1", count)
	}

	got, err = s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List after purge: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 entries after purge, got %d", len(got))
	}
}

// Verifies the store works with an in-memory SQLite database path.
func TestInMemoryMode(t *testing.T) {
	s, err := sqlitedlq.NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore(:memory:): %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)

	if err := s.Write(ctx, makeEntry("mem-1", "route-A", "cat", now)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].ID != "mem-1" {
		t.Fatalf("ID: got %q, want %q", got[0].ID, "mem-1")
	}
}

// Verifies data survives closing the database and opening the same file again.
func TestDurability_CloseAndReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "durable.db")

	s1, err := sqlitedlq.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}

	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	entry := makeEntry("dur-1", "route-A", "cat", now)

	if err := s1.Write(ctx, entry); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	s2, err := sqlitedlq.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, err := s2.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry after reopen, got %d", len(got))
	}
	if got[0].ID != "dur-1" {
		t.Fatalf("ID: got %q, want %q", got[0].ID, "dur-1")
	}
	if !got[0].FailedAt.Equal(now) {
		t.Fatalf("FailedAt: got %v, want %v", got[0].FailedAt, now)
	}
	if string(got[0].Envelope.Payload) != `{"key":"value"}` {
		t.Fatalf("Envelope.Payload: got %q", got[0].Envelope.Payload)
	}
}

// Verifies the database file remains on disk after Close.
func TestFileExistsAfterClose(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exists.db")

	s, err := sqlitedlq.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	if err := s.Write(ctx, makeEntry("fe-1", "route-A", "cat", now)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatal("db file should exist after close")
	}
}

// Verifies List returns entries ordered by FailedAt descending.
func TestListOrderByFailedAtDesc(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Millisecond)
	t1 := now.Add(-2 * time.Hour)
	t2 := now.Add(-1 * time.Hour)
	t3 := now

	mustWrite(t, s, ctx, makeEntry("ord-1", "route-A", "cat", t1))
	mustWrite(t, s, ctx, makeEntry("ord-2", "route-A", "cat", t2))
	mustWrite(t, s, ctx, makeEntry("ord-3", "route-A", "cat", t3))

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}

	if got[0].ID != "ord-3" {
		t.Fatalf("first entry should be newest (ord-3), got %q", got[0].ID)
	}
	if got[1].ID != "ord-2" {
		t.Fatalf("second entry should be middle (ord-2), got %q", got[1].ID)
	}
	if got[2].ID != "ord-1" {
		t.Fatalf("third entry should be oldest (ord-1), got %q", got[2].ID)
	}

	for i := 1; i < len(got); i++ {
		if got[i].FailedAt.After(got[i-1].FailedAt) {
			t.Fatalf("entries not in descending order: [%d].FailedAt=%v > [%d].FailedAt=%v",
				i, got[i].FailedAt, i-1, got[i-1].FailedAt)
		}
	}
}

// Verifies Delete silently skips non-existent IDs and returns count=0.
func TestDeleteNonExistentEntry(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	count, err := s.Delete(ctx, []string{"no-such-id"})
	if err != nil {
		t.Fatalf("Delete: unexpected error %v", err)
	}
	if count != 0 {
		t.Fatalf("Delete count: got %d, want 0", count)
	}
}

// Verifies List on a new store returns an empty non-nil slice.
func TestListEmptyStore(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	got, err := s.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(got))
	}
}

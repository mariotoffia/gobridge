package dynamodbdlq_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports/storetest"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// Verifies the DynamoDB DLQ store passes the shared DLQ store conformance suite.
func TestDLQStoreConformance(t *testing.T) {
	store := newStore(t, "dlq-conf")
	storetest.RunDLQStoreTests(t, store)
}

func TestMain(m *testing.M) {
	code := m.Run()
	ddblocal.Shutdown()
	os.Exit(code)
}

func makeEntry(id, routeID, category string, failedAt time.Time) domain.DLQEntry {
	return domain.DLQEntry{
		ID: id,
		Envelope: domain.Envelope{
			ID:      "env-" + id,
			Subject: "test/subject",
			Payload: []byte(`{"key":"value"}`),
			Headers: map[string]any{"h1": "v1"},
		},
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
	}
}

func newStore(t *testing.T, prefix string) *dynamodbdlq.Store {
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

// Verifies Write persists a DLQ entry and List returns equivalent fields and envelope data.
func TestWriteAndList(t *testing.T) {
	store := newStore(t, "dlq-wal")
	ctx := context.Background()

	failedAt := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	entry := makeEntry("wal-1", "route-A", "timeout", failedAt)

	if err := store.Write(ctx, entry); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := store.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	got := entries[0]
	if got.ID != "wal-1" {
		t.Errorf("ID: got %q, want %q", got.ID, "wal-1")
	}
	if got.RouteID != "route-A" {
		t.Errorf("RouteID: got %q, want %q", got.RouteID, "route-A")
	}
	if got.Category != "timeout" {
		t.Errorf("Category: got %q, want %q", got.Category, "timeout")
	}
	if !got.FailedAt.Equal(failedAt) {
		t.Errorf("FailedAt: got %v, want %v", got.FailedAt, failedAt)
	}
	if got.BindingID != "bind-wal-1" {
		t.Errorf("BindingID: got %q, want %q", got.BindingID, "bind-wal-1")
	}
	if got.SessionID != "sess-wal-1" {
		t.Errorf("SessionID: got %q, want %q", got.SessionID, "sess-wal-1")
	}
	if got.SourceID != "src-wal-1" {
		t.Errorf("SourceID: got %q, want %q", got.SourceID, "src-wal-1")
	}
	if got.CorrelationID != "corr-wal-1" {
		t.Errorf("CorrelationID: got %q, want %q", got.CorrelationID, "corr-wal-1")
	}
	if got.Reason != "test failure" {
		t.Errorf("Reason: got %q, want %q", got.Reason, "test failure")
	}
	if got.ErrorCode != "TEST_ERROR" {
		t.Errorf("ErrorCode: got %q, want %q", got.ErrorCode, "TEST_ERROR")
	}
	if got.LastError != "something went wrong" {
		t.Errorf("LastError: got %q, want %q", got.LastError, "something went wrong")
	}
	if got.Attempts != 3 {
		t.Errorf("Attempts: got %d, want %d", got.Attempts, 3)
	}

	if got.Envelope.ID != "env-wal-1" {
		t.Errorf("Envelope.ID: got %q, want %q", got.Envelope.ID, "env-wal-1")
	}
	if got.Envelope.Subject != "test/subject" {
		t.Errorf("Envelope.Subject: got %q, want %q", got.Envelope.Subject, "test/subject")
	}
	if !bytes.Equal(got.Envelope.Payload, []byte(`{"key":"value"}`)) {
		t.Errorf("Envelope.Payload: got %q, want %q", got.Envelope.Payload, `{"key":"value"}`)
	}
	if len(got.Envelope.Headers) != 1 {
		t.Fatalf("Envelope.Headers length: got %d, want 1", len(got.Envelope.Headers))
	}
	if got.Envelope.Headers["h1"] != "v1" {
		t.Errorf("Envelope.Headers[h1]: got %v, want %q", got.Envelope.Headers["h1"], "v1")
	}
}

// Verifies List filters results by RouteID.
func TestListFilterByRouteID(t *testing.T) {
	store := newStore(t, "dlq-frid")
	ctx := context.Background()
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("fr-1", "route-A", "timeout", base),
		makeEntry("fr-2", "route-B", "timeout", base.Add(1*time.Minute)),
		makeEntry("fr-3", "route-A", "timeout", base.Add(2*time.Minute)),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	entries, err := store.List(ctx, domain.DLQFilter{RouteID: "route-A"})
	if err != nil {
		t.Fatalf("list: %v", err)
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

// Verifies List filters results by Category.
func TestListFilterByCategory(t *testing.T) {
	store := newStore(t, "dlq-fcat")
	ctx := context.Background()
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("fc-1", "route-A", "timeout", base),
		makeEntry("fc-2", "route-A", "schema", base.Add(1*time.Minute)),
		makeEntry("fc-3", "route-A", "timeout", base.Add(2*time.Minute)),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	entries, err := store.List(ctx, domain.DLQFilter{Category: "schema"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ID != "fc-2" {
		t.Errorf("expected entry fc-2, got %q", entries[0].ID)
	}
}

// Verifies List includes only entries with FailedAt on or after Since.
func TestListFilterBySince(t *testing.T) {
	store := newStore(t, "dlq-since")
	ctx := context.Background()

	t1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("fs-1", "route-A", "timeout", t1),
		makeEntry("fs-2", "route-A", "timeout", t2),
		makeEntry("fs-3", "route-A", "timeout", t3),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	entries, err := store.List(ctx, domain.DLQFilter{Since: t2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (at or after t2), got %d", len(entries))
	}
	for _, e := range entries {
		if e.FailedAt.Before(t2) {
			t.Errorf("entry %q has FailedAt %v, before Since %v", e.ID, e.FailedAt, t2)
		}
	}
}

// Verifies List includes only entries with FailedAt strictly before Before.
func TestListFilterByBefore(t *testing.T) {
	store := newStore(t, "dlq-before")
	ctx := context.Background()

	t1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("fb-1", "route-A", "timeout", t1),
		makeEntry("fb-2", "route-A", "timeout", t2),
		makeEntry("fb-3", "route-A", "timeout", t3),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	entries, err := store.List(ctx, domain.DLQFilter{Before: t2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (strictly before t2), got %d", len(entries))
	}
	if entries[0].ID != "fb-1" {
		t.Errorf("expected entry fb-1, got %q", entries[0].ID)
	}
}

// Verifies List honors the Limit field on the filter.
func TestListRespectsLimit(t *testing.T) {
	store := newStore(t, "dlq-limit")
	ctx := context.Background()
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		e := makeEntry(
			fmt.Sprintf("lim-%d", i), "route-A", "timeout",
			base.Add(time.Duration(i)*time.Minute),
		)
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	entries, err := store.List(ctx, domain.DLQFilter{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

// Verifies writing the same entry ID twice returns ErrDuplicateRecord.
func TestWriteIdempotent(t *testing.T) {
	store := newStore(t, "dlq-idem")
	ctx := context.Background()

	entry := makeEntry("idem-1", "route-A", "timeout",
		time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC))

	if err := store.Write(ctx, entry); err != nil {
		t.Fatalf("first write: %v", err)
	}

	err := store.Write(ctx, entry)
	if !errors.Is(err, domain.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord, got %v", err)
	}
}

// Verifies Replay succeeds for multiple IDs and entries remain listable.
func TestReplayMarksEntries(t *testing.T) {
	store := newStore(t, "dlq-replay")
	ctx := context.Background()
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("rpl-1", "route-A", "timeout", base),
		makeEntry("rpl-2", "route-A", "timeout", base.Add(1*time.Minute)),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	if err := store.Replay(ctx, []string{"rpl-1", "rpl-2"}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	entries, err := store.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("list after replay: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries after replay, got %d", len(entries))
	}
}

// Verifies Purge removes entries older than the cutoff and reports the count removed.
func TestPurgeRemovesOld(t *testing.T) {
	store := newStore(t, "dlq-purgeold")
	ctx := context.Background()

	old := time.Date(2024, 1, 10, 10, 0, 0, 0, time.UTC)
	mid := time.Date(2024, 1, 12, 10, 0, 0, 0, time.UTC)
	recent := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("po-old", "route-A", "timeout", old),
		makeEntry("po-mid", "route-A", "timeout", mid),
		makeEntry("po-new", "route-A", "timeout", recent),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	cutoff := time.Date(2024, 1, 13, 0, 0, 0, 0, time.UTC)
	n, err := store.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged count: got %d, want 2", n)
	}

	entries, err := store.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("list after purge: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 remaining entry, got %d", len(entries))
	}
	if entries[0].ID != "po-new" {
		t.Errorf("remaining entry: got %q, want %q", entries[0].ID, "po-new")
	}
}

// Verifies Purge only deletes entries before the cutoff while leaving newer rows.
func TestPurgeSkipsRecent(t *testing.T) {
	store := newStore(t, "dlq-purgerec")
	ctx := context.Background()

	old := time.Date(2024, 1, 10, 10, 0, 0, 0, time.UTC)
	recent := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	future := time.Date(2024, 1, 20, 10, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("ps-1", "route-A", "timeout", old),
		makeEntry("ps-2", "route-A", "timeout", recent),
		makeEntry("ps-3", "route-A", "timeout", future),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	cutoff := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC)
	n, err := store.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged count: got %d, want 1", n)
	}

	entries, err := store.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("list after purge: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 remaining entries, got %d", len(entries))
	}
}

// Verifies write, list, replay, and purge interact correctly in a combined scenario.
func TestFullLifecycle(t *testing.T) {
	store := newStore(t, "dlq-lifecycle")
	ctx := context.Background()

	t1 := time.Date(2024, 1, 10, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 12, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2024, 1, 14, 10, 0, 0, 0, time.UTC)
	t4 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("lc-1", "route-A", "timeout", t1),
		makeEntry("lc-2", "route-A", "schema", t2),
		makeEntry("lc-3", "route-B", "timeout", t3),
		makeEntry("lc-4", "route-B", "auth", t4),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	entries, err := store.List(ctx, domain.DLQFilter{})
	if err != nil {
		t.Fatalf("initial list: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	if err := store.Replay(ctx, []string{"lc-1", "lc-3"}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	cutoff := time.Date(2024, 1, 13, 0, 0, 0, 0, time.UTC)
	n, err := store.Purge(ctx, cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged count: got %d, want 2", n)
	}

	entries, err = store.List(ctx, domain.DLQFilter{})
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
	if !ids["lc-3"] {
		t.Error("expected lc-3 to remain")
	}
	if !ids["lc-4"] {
		t.Error("expected lc-4 to remain")
	}
}

// Verifies EnsureTable succeeds when invoked repeatedly for the same store.
func TestEnsureTableIdempotent(t *testing.T) {
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("dlq-etable")
	store := dynamodbdlq.NewStore(client, dynamodbdlq.WithTableName(tableName))
	ctx := context.Background()

	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("first EnsureTable: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("second EnsureTable should be idempotent: %v", err)
	}
}

// Verifies List applies RouteID and Category filters together.
func TestListBothRouteAndCategory(t *testing.T) {
	store := newStore(t, "dlq-both")
	ctx := context.Background()
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	for _, e := range []domain.DLQEntry{
		makeEntry("rc-1", "route-A", "timeout", base),
		makeEntry("rc-2", "route-A", "schema", base.Add(1*time.Minute)),
		makeEntry("rc-3", "route-B", "timeout", base.Add(2*time.Minute)),
		makeEntry("rc-4", "route-B", "schema", base.Add(3*time.Minute)),
	} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %q: %v", e.ID, err)
		}
	}

	entries, err := store.List(ctx, domain.DLQFilter{
		RouteID:  "route-A",
		Category: "timeout",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ID != "rc-1" {
		t.Errorf("expected entry rc-1, got %q", entries[0].ID)
	}
}

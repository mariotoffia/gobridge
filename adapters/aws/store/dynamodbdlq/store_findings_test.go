package dynamodbdlq_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// newDLQStoreClient builds a ddblocal-backed store with the given options and
// returns the raw client + table name so a test can inspect/seed storage
// directly (e.g. observe native Scan order, which is stable per key set but
// uncorrelated with failed_at).
func newDLQStoreClient(t *testing.T, prefix string, opts ...dynamodbdlq.Option) (*dynamodbdlq.Store, *dynamodb.Client, string) {
	t.Helper()
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable(prefix)
	all := append([]dynamodbdlq.Option{dynamodbdlq.WithTableName(table)}, opts...)
	store := dynamodbdlq.NewStore(client, all...)
	if err := store.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)
	return store, client, table
}

// rawScanIDs returns the DLQ entry IDs (DLQ# prefix stripped) in DynamoDB's
// native Scan order for the table. That order is stable for a fixed key set
// (verified) but hash-based and uncorrelated with failed_at, so tests use it to
// place the globally-oldest / matching entries AFTER the first scan page(s) —
// the exact layout the pre-fix truncating/bounded code mishandled.
func rawScanIDs(t *testing.T, client *dynamodb.Client, table string) []string {
	t.Helper()
	out, err := client.Scan(context.Background(), &dynamodb.ScanInput{TableName: &table})
	if err != nil {
		t.Fatalf("raw scan: %v", err)
	}
	ids := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		if v, ok := it["PK"].(*ddbtypes.AttributeValueMemberS); ok && len(v.Value) > 4 {
			ids = append(ids, v.Value[4:])
		}
	}
	return ids
}

func rawDelete(t *testing.T, client *dynamodb.Client, table, id string) {
	t.Helper()
	_, err := client.DeleteItem(context.Background(), &dynamodb.DeleteItemInput{
		TableName: &table,
		Key:       map[string]ddbtypes.AttributeValue{"PK": &ddbtypes.AttributeValueMemberS{Value: "DLQ#" + id}},
	})
	if err != nil {
		t.Fatalf("raw delete %s: %v", id, err)
	}
}

func idsOfEntries(es []routing.DLQEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID()
	}
	return out
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestWriteMetadataOnlyEntryRoundTrips is the c09-1 regression: a metadata-only
// DLQ entry (no envelope) must round-trip through Write→read without an
// envelope-unmarshal error.
//
// Root cause: added a mandatory-ID guard to Envelope.UnmarshalJSON. A
// metadata-only entry's zero Snapshot marshals to a NON-empty zero-JSON
// ({"CreatedAt":…}) carrying an empty envelope ID; the read guard only skips
// unmarshal on the empty string "", not on that zero-JSON, so read-back FAILED.
// The fix writes "" for an empty envelope (mirroring sqlitedlq).
//
// Counterfactual: on the pre-fix code the List calls below return
// "unmarshal envelope: … envelope ID is required".
func TestWriteMetadataOnlyEntryRoundTrips(t *testing.T) {
	store := newStore(t, "dlq-c09")
	ctx := context.Background()

	failedAt := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:       "meta-only-1",
		RouteID:  "route-Z",
		Category: "poison",
		Reason:   "no envelope could be decoded",
		FailedAt: failedAt,
		Attempts: 1,
	})
	if id := entry.Snapshot().ID(); id != "" {
		t.Fatalf("precondition: metadata-only entry must have an empty envelope ID, got %q", id)
	}

	if err := store.Write(ctx, entry); err != nil {
		t.Fatalf("write metadata-only entry: %v", err)
	}

	// Scan read path (listByScan → unmarshalEntry).
	entries, err := store.List(ctx, routing.DLQFilter{})
	if err != nil {
		t.Fatalf("List (scan) after metadata-only write — c09-1 read-back guard: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry from scan, got %d", len(entries))
	}
	assertMetadataOnly(t, entries[0])

	// GSI read path (listByIndex → unmarshalEntry).
	byRoute, err := store.List(ctx, routing.DLQFilter{RouteID: "route-Z"})
	if err != nil {
		t.Fatalf("List (RouteIndex) after metadata-only write — c09-1 read-back guard: %v", err)
	}
	if len(byRoute) != 1 {
		t.Fatalf("expected 1 entry from RouteIndex, got %d", len(byRoute))
	}
	assertMetadataOnly(t, byRoute[0])
}

func assertMetadataOnly(t *testing.T, got routing.DLQEntry) {
	t.Helper()
	if got.ID() != "meta-only-1" || got.RouteID() != "route-Z" || got.Category() != "poison" {
		t.Fatalf("metadata mismatch on read-back: id=%q route=%q cat=%q", got.ID(), got.RouteID(), got.Category())
	}
	if got.Snapshot().ID() != "" {
		t.Fatalf("expected empty envelope on read-back, got envelope ID %q", got.Snapshot().ID())
	}
}

// TestListSelectsGlobalOldestAcrossScanPages is the MEDIUM regression: an
// unfiltered List must return the GLOBALLY oldest entries by failed_at, not the
// oldest among the first `limit` items in DynamoDB's arbitrary Scan order.
//
// Adversarial layout: the entry DynamoDB scans FIRST is stamped NEWEST and the
// one scanned LAST is stamped OLDEST (ages assigned after observing the stable
// scan order). scanPageSize=1 forces one item per page.
//
// Counterfactual: the former `for len(entries) < limit` loop stopped after
// collecting `limit` items in Scan order and sorted only that subset, so it
// returned the first-`limit`-scanned (here the NEWEST) — this test asserts the
// globally oldest and fails on that code.
func TestListSelectsGlobalOldestAcrossScanPages(t *testing.T) {
	store, client, table := newDLQStoreClient(t, "dlq-oldest", dynamodbdlq.WithScanPageSize(1))
	ctx := context.Background()

	const n = 8
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < n; i++ {
		if err := store.Write(ctx, makeEntry(fmt.Sprintf("age-%02d", i), "", "", base)); err != nil {
			t.Fatalf("seed write %d: %v", i, err)
		}
	}
	order := rawScanIDs(t, client, table)
	if len(order) != n {
		t.Fatalf("expected %d seeded entries, got %d", n, len(order))
	}

	// Reassign ages: scan position 0 = newest … position n-1 = oldest.
	ageByID := make(map[string]time.Time, n)
	for pos, id := range order {
		ageByID[id] = base.Add(time.Duration(n-1-pos) * time.Minute)
		rawDelete(t, client, table, id)
	}
	for _, id := range order {
		if err := store.Write(ctx, makeEntry(id, "", "", ageByID[id])); err != nil {
			t.Fatalf("rewrite %s: %v", id, err)
		}
	}
	// Oldest-first == reverse of scan order (by construction of ageByID).
	oldestFirst := make([]string, n)
	for pos, id := range order {
		oldestFirst[n-1-pos] = id
	}

	got, err := store.List(ctx, routing.DLQFilter{Limit: 3})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	gotIDs := idsOfEntries(got)
	want := oldestFirst[:3]
	if !equalStrs(gotIDs, want) {
		t.Fatalf("List(limit=3) must return the GLOBALLY oldest 3 by failed_at\n got=%v\nwant=%v\n(scan order=%v; pre-fix returned the first-3-scanned = newest)",
			gotIDs, want, order)
	}
}

// TestDeleteByFilterUnlimitedExhaustsBeyondScanBound is the MEDIUM regression:
// DeleteByFilter with Limit<=0 must delete EVERY matching entry even when the
// matches sit behind more than maxScanPages of non-matching entries.
//
// Adversarial layout (maxScanPages=1, scanPageSize=1): the FIRST-scanned entry
// is non-matching (new) and every other entry matches (old). The former bounded
// List examines only that first non-matching entry, returns empty, and
// DeleteByFilter stops having deleted nothing.
//
// Counterfactual: routing the unlimited index-less delete through the bounded
// List path (pre-fix) deletes 0 here; the exhaustive scan-delete deletes all
// n-1 matches.
func TestDeleteByFilterUnlimitedExhaustsBeyondScanBound(t *testing.T) {
	store, client, table := newDLQStoreClient(t, "dlq-delall",
		dynamodbdlq.WithMaxScanPages(1), dynamodbdlq.WithScanPageSize(1))
	ctx := context.Background()

	const n = 6
	base := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		if err := store.Write(ctx, makeEntry(fmt.Sprintf("del-%02d", i), "", "", base)); err != nil {
			t.Fatalf("seed write %d: %v", i, err)
		}
	}
	order := rawScanIDs(t, client, table)
	if len(order) != n {
		t.Fatalf("expected %d seeded entries, got %d", n, len(order))
	}

	before := base.Add(1 * time.Hour)
	newAge := base.Add(2 * time.Hour) // >= before → non-matching
	oldAge := base                    // <  before → matching
	first := order[0]

	for _, id := range order {
		rawDelete(t, client, table, id)
	}
	for _, id := range order {
		age := oldAge
		if id == first {
			age = newAge
		}
		if err := store.Write(ctx, makeEntry(id, "", "", age)); err != nil {
			t.Fatalf("rewrite %s: %v", id, err)
		}
	}

	count, err := store.DeleteByFilter(ctx, routing.DLQFilter{Before: before})
	if err != nil {
		t.Fatalf("delete_by_filter: %v", err)
	}
	if count != n-1 {
		t.Fatalf("DeleteByFilter(Limit<=0) must delete EVERY matching entry: got %d, want %d "+
			"(pre-fix bounded path stops behind the first non-matching scan page)", count, n-1)
	}

	remaining := rawScanIDs(t, client, table)
	if len(remaining) != 1 || remaining[0] != first {
		t.Fatalf("only the non-matching entry %q must remain, got %v", first, remaining)
	}
}

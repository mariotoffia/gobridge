package dynamodboutbox

// Internal tests for the ordering-key claim path and the short-batch rule. They
// drive the fakeDDB seam because neither behaviour is reproducible against
// DynamoDB Local: its global secondary indexes are synchronously consistent, so
// index lag cannot be observed there, and a mid-batch throttle cannot be
// provoked on demand.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// keyedQueryItem builds a pending record item carrying an ordering key at an
// explicit claim position, so a test can place siblings in a known age order.
func keyedQueryItem(pk, sk, recordID, orderingKey string, createdAtMs int64, seq uint64) map[string]ddbtypes.AttributeValue {
	item := pendingQueryItem(pk, sk, recordID)
	item["created_at"] = &ddbtypes.AttributeValueMemberN{Value: i64(createdAtMs)}
	item["seq"] = &ddbtypes.AttributeValueMemberN{Value: u64(seq)}
	item["envelope_id"] = &ddbtypes.AttributeValueMemberS{Value: "env-" + recordID}
	if orderingKey != "" {
		item[attrOrderingKey] = &ddbtypes.AttributeValueMemberS{Value: orderingKey}
	}
	return item
}

// claimedQueryItem builds a record already CLAIMED at claimVersion by owner —
// the stranded head a failed release or a dead owner leaves behind.
func claimedQueryItem(
	pk, sk, recordID, orderingKey string,
	createdAtMs int64, seq uint64,
	owner string, claimVersion uint64, claimedAtMs int64,
) map[string]ddbtypes.AttributeValue {
	item := keyedQueryItem(pk, sk, recordID, orderingKey, createdAtMs, seq)
	item["status"] = &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)}
	item["claimed_by"] = &ddbtypes.AttributeValueMemberS{Value: owner}
	item["claim_version"] = &ddbtypes.AttributeValueMemberN{Value: u64(claimVersion)}
	item["claimed_at"] = &ddbtypes.AttributeValueMemberN{Value: i64(claimedAtMs)}
	return item
}

// TestClaim_OrderingKeyedRecords_BypassLaggingIndexForConsistentScan is the
// GSI-lag regression. A ClaimIndex query is eventually consistent and its
// propagation is per item and unordered, so two same-key records written
// microseconds apart can surface in the WRONG order: the index shows the
// younger sibling while the older one is still propagating. Claiming in index
// order would send the younger message first with zero failures anywhere.
//
// The fake serves exactly that state — the index knows only the younger B, the
// base table (ConsistentRead) holds both A and B — and the claim must land on
// the base table and return A first.
//
// Mutation this kills: claiming keyed candidates straight off the index →
// the base table is never queried and B comes back first → this test FAILs.
func TestClaim_OrderingKeyedRecords_BypassLaggingIndexForConsistentScan(t *testing.T) {
	const (
		partition = "PART#ordered"
		key       = "device-7"
		baseMs    = int64(1_700_000_000_000)
	)
	older := keyedQueryItem(partition, "OUTBOX#env-A#bind", "rec-A", key, baseMs, 1)
	younger := keyedQueryItem(partition, "OUTBOX#env-B#bind", "rec-B", key, baseMs+10, 2)

	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName != nil && *in.IndexName == claimIndexName {
			// The lagging index has only propagated the YOUNGER sibling.
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{younger}}, nil
		}
		if in.ConsistentRead == nil || !*in.ConsistentRead {
			t.Fatal("the ordering-key claim path must read the base table with ConsistentRead")
		}
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{younger, older}}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got := f.queryCalls[""]; got == 0 {
		t.Fatal("a keyed candidate must send the claim to the strongly consistent base-table scan")
	}
	if len(claimed) != 2 {
		t.Fatalf("expected both siblings claimed, got %d", len(claimed))
	}
	if claimed[0].ID() != "rec-A" || claimed[1].ID() != "rec-B" {
		t.Fatalf("same-key records must come back oldest-first, got %s then %s",
			claimed[0].ID(), claimed[1].ID())
	}
}

// TestClaim_StrandedHead_BlocksYoungerSiblingOnScanPath proves the DynamoDB
// scan path evaluates the head-of-line rule against records it cannot claim: a
// head left Claimed at the SAME fence version by a dead owner is not a
// candidate, but it must still stall its younger sibling. The scan therefore
// has to read every NON-TERMINAL record, not just the claimable ones.
func TestClaim_StrandedHead_BlocksYoungerSiblingOnScanPath(t *testing.T) {
	const (
		partition = "PART#stranded"
		key       = "device-9"
		baseMs    = int64(1_700_000_000_000)
	)
	// Claimed at the same version this claim carries, and freshly claimed, so
	// neither version nor staleness makes it reclaimable.
	head := claimedQueryItem(partition, "OUTBOX#env-H#bind", "rec-H", key, baseMs, 1,
		"other-owner", 1, time.Now().UnixMilli())
	tail := keyedQueryItem(partition, "OUTBOX#env-T#bind", "rec-T", key, baseMs+10, 2)
	loose := keyedQueryItem(partition, "OUTBOX#env-K#bind", "rec-K", "", baseMs+20, 3)

	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName != nil && *in.IndexName == claimIndexName {
			// The index filter hides the stranded head (it is not claimable);
			// the keyed tail is what pushes the claim onto the scan path.
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{tail, loose}}, nil
		}
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{head, tail, loose}}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID() != "rec-K" {
		ids := make([]string, 0, len(claimed))
		for _, r := range claimed {
			ids = append(ids, r.ID())
		}
		t.Fatalf("got %v, want [rec-K] (rec-T must not overtake the stranded rec-H)", ids)
	}
}

// TestClaim_MidBatchFailure_ReturnsAlreadyClaimedRecords is the stranded-claim
// regression. Claim issues one transaction per record, so a throttle — or the
// claim's own deadline — can land after earlier records are already durably
// claimed. Discarding them (returning them alongside an error the drainer
// throws away) leaves them Claimed and invisible to CountPending until the
// wall-clock stale window, charging each recovery cycle a replay attempt, so a
// short send_timeout relative to claim cost eventually poisons them to the
// dead-letter queue WITHOUT A SINGLE SEND. A short batch is legal; losing the
// batch is not.
//
// Mutation this kills: returning (nil, err) once anything is claimed → the
// claimed slice is empty and the error non-nil → this test FAILs.
func TestClaim_MidBatchFailure_ReturnsAlreadyClaimedRecords(t *testing.T) {
	const (
		partition = "PART#truncate"
		total     = 100
		failAt    = 60
	)
	items := make([]map[string]ddbtypes.AttributeValue, total)
	for i := range items {
		items[i] = keyedQueryItem(partition,
			fmt.Sprintf("OUTBOX#env-%03d#bind", i), fmt.Sprintf("rec-%03d", i),
			"", int64(1_700_000_000_000+i), uint64(i+1))
	}

	tests := []struct {
		name string
		fail func(cancel context.CancelFunc) error
	}{
		{
			name: "throttled transaction",
			fail: func(context.CancelFunc) error {
				return transactCanceled(ccReasonNone, "ProvisionedThroughputExceeded")
			},
		},
		{
			name: "claim deadline expires",
			fail: func(cancel context.CancelFunc) error {
				cancel()
				return context.Canceled
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			f := newFakeDDB()
			f.getItemFn = fenceGetItem("0")
			f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				return &dynamodb.QueryOutput{Items: items}, nil
			}
			calls := 0
			f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
				calls++
				if calls > failAt-1 {
					return nil, tc.fail(cancel)
				}
				return &dynamodb.TransactWriteItemsOutput{}, nil
			}
			rec := &ports.RecordingExporter{}
			s := newFakeStore(f)
			s.metrics = rec

			claimed, err := s.Claim(ctx, partition,
				persistence.LeaseToken{Version: 1, Owner: "drainer"}, total)
			if err != nil {
				t.Fatalf("a mid-batch transient failure must return a SHORT batch, not an error: %v", err)
			}
			if len(claimed) != failAt-1 {
				t.Fatalf("expected the %d records already claimed, got %d", failAt-1, len(claimed))
			}
			if got := len(rec.FindEntries(MetricClaimTruncated)); got != 1 {
				t.Fatalf("a truncated claim must be counted once via %s, got %d", MetricClaimTruncated, got)
			}
		})
	}
}

// A mid-batch ErrStaleFencingToken is the ONE failure that still surfaces with
// no records: this owner has lost the partition, so it must stop and re-fence
// rather than send work a successor now owns.
func TestClaim_MidBatchFenceLoss_SurfacesErrorWithNoRecords(t *testing.T) {
	const partition = "PART#fence-loss"
	items := []map[string]ddbtypes.AttributeValue{
		keyedQueryItem(partition, "OUTBOX#env-1#bind", "rec-1", "", 1_700_000_000_000, 1),
		keyedQueryItem(partition, "OUTBOX#env-2#bind", "rec-2", "", 1_700_000_000_001, 2),
	}

	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: items}, nil
	}
	calls := 0
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		calls++
		if calls == 1 {
			return &dynamodb.TransactWriteItemsOutput{}, nil
		}
		return nil, transactCanceled(ccReasonCondCheckFailed, ccReasonNone)
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("a lost partition must surface ErrStaleFencingToken, got %v", err)
	}
	if claimed != nil {
		t.Fatalf("no records may be returned once the partition is lost, got %d", len(claimed))
	}
}

// TestExpire_SecondPageQueryFails_ReportsFirstPageCount pins the conservation
// law across a truncated sweep: records flipped on page 1 are terminal and will
// never be counted again, so a failure fetching page 2 must still report them.
// Dropping the count silently under-reports MessagesExpired and the
// received = sent + dropped + filtered + expired + dlq + inflight identity stops
// closing.
func TestExpire_SecondPageQueryFails_ReportsFirstPageCount(t *testing.T) {
	const partition = "PART#expire-pages"
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	token := persistence.LeaseToken{Version: 5, Owner: "a"}
	before := time.UnixMilli(1_700_000_100_000)

	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.ExclusiveStartKey == nil {
			return &dynamodb.QueryOutput{
				Items:            []map[string]ddbtypes.AttributeValue{pendingQueryItem(partition, "OUTBOX#env-1#bind-1", "rec-1")},
				LastEvaluatedKey: lastKey("OUTBOX#env-1#bind-1"),
			}, nil
		}
		return nil, errors.New("throttled reading page 2")
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	s := newFakeStore(f)

	n, err := s.Expire(context.Background(), before, partition, token)
	if err == nil {
		t.Fatal("a failed page fetch must surface an error")
	}
	if n != 1 {
		t.Fatalf("a sweep truncated by a page failure must report what it did expire, got %d", n)
	}
}

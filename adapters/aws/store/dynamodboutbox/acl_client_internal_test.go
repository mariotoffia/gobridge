package dynamodboutbox

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// fakeDDB is an in-package dynamoAPI seam that lets unit tests drive the
// outbox store's control flow (fence reads/writes, GSI lookups) without a
// live DynamoDB and count the calls made on each path.
type fakeDDB struct {
	mu sync.Mutex

	getItemFn    func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	updateItemFn func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
	queryFn      func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error)

	getItemCalls int
	// queryCalls counts calls per index name ("" == base table).
	queryCalls map[string]int
}

func newFakeDDB() *fakeDDB { return &fakeDDB{queryCalls: map[string]int{}} }

func (f *fakeDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	f.getItemCalls++
	fn := f.getItemFn
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	fn := f.updateItemFn
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *fakeDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.mu.Lock()
	idx := ""
	if in.IndexName != nil {
		idx = *in.IndexName
	}
	f.queryCalls[idx]++
	fn := f.queryFn
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return &dynamodb.QueryOutput{}, nil
}

func (f *fakeDDB) PutItem(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDDB) TransactWriteItems(_ context.Context, _ *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func (f *fakeDDB) CreateTable(_ context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return &dynamodb.CreateTableOutput{}, nil
}

func (f *fakeDDB) DescribeTable(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}

func (f *fakeDDB) UpdateTimeToLive(_ context.Context, _ *dynamodb.UpdateTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error) {
	return &dynamodb.UpdateTimeToLiveOutput{}, nil
}

func newFakeStore(f *fakeDDB) *Store {
	return &Store{
		client:             f,
		table:              "outbox-test",
		compactGrace:       time.Hour,
		staleClaim:         30 * time.Second,
		clk:                clock.System,
		resolveMaxAttempts: defaultResolveMaxAttempts,
		resolveBackoff:     0, // deterministic: no wall-clock backoff in tests
		keyCache:           map[string]recordKey{},
	}
}

// Regression for J5: reading the monotonic claim fence must be an O(1)
// single GetItem on the per-partition fence row, not a full-partition scan.
func TestClaim_FenceRead_IsSingleGetItem(t *testing.T) {
	f := newFakeDDB()
	// Fence row present with version 3.
	f.getItemFn = func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
			attrMaxClaimVersion: &ddbtypes.AttributeValueMemberN{Value: "3"},
		}}, nil
	}
	// No pending/claimable records.
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{}, nil
	}
	s := newFakeStore(f)

	// A stale token (version 2 < fence 3) is rejected without any scan.
	_, err := s.Claim(context.Background(), "PART#1", persistence.LeaseToken{Version: 2, Owner: "o"}, 10)
	if err == nil {
		t.Fatalf("expected stale-fencing rejection")
	}
	if f.getItemCalls != 1 {
		t.Fatalf("fence read should be a single GetItem, got %d", f.getItemCalls)
	}
	if got := f.queryCalls[""]; got != 0 {
		t.Fatalf("stale-token rejection must not scan the partition; base-table queries = %d", got)
	}
}

// Regression for J2: Complete addresses records via the Claim-populated key
// cache and never touches the eventually consistent RecordIDIndex GSI on the
// happy path (which could lag and report not-found → duplicate delivery).
func TestComplete_UsesCachedKeys_NoGSILookup(t *testing.T) {
	f := newFakeDDB()
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		t.Fatalf("Complete must not query the GSI when keys are cached")
		return nil, nil
	}
	s := newFakeStore(f)
	s.cacheKey("rec-1", "PART#1", "OUTBOX#env#bind")

	err := s.Complete(context.Background(), []string{"rec-1"}, persistence.LeaseToken{Version: 1, Owner: "o"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := f.queryCalls["RecordIDIndex"]; got != 0 {
		t.Fatalf("expected 0 GSI lookups, got %d", got)
	}
	// Terminal record's keys are evicted.
	if _, ok := s.lookupKey("rec-1"); ok {
		t.Fatalf("cached keys should be evicted after Complete")
	}
}

// Regression for J2: on a cold key cache Complete retries the lagging GSI up
// to the configured bound instead of giving up after a single not-found and
// duplicating the message later.
func TestComplete_RetriesLaggingGSI(t *testing.T) {
	f := newFakeDDB()
	var gsiCalls int
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		gsiCalls++
		if gsiCalls < 3 {
			return &dynamodb.QueryOutput{}, nil // GSI lag: not yet visible
		}
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{{
			"PK": &ddbtypes.AttributeValueMemberS{Value: "PART#1"},
			"SK": &ddbtypes.AttributeValueMemberS{Value: "OUTBOX#env#bind"},
		}}}, nil
	}
	s := newFakeStore(f) // resolveBackoff 0 → deterministic, no sleeping

	err := s.Complete(context.Background(), []string{"rec-cold"}, persistence.LeaseToken{Version: 1, Owner: "o"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gsiCalls != 3 {
		t.Fatalf("expected 3 GSI attempts (2 lagging + 1 hit), got %d", gsiCalls)
	}
}

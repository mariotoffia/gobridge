package dynamodboutbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// fakeDDB is an in-package dynamoAPI seam that lets unit tests drive the
// outbox store's control flow (fence reads/writes, GSI lookups) without a
// live DynamoDB and count the calls made on each path.
type fakeDDB struct {
	mu sync.Mutex

	getItemFn       func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	updateItemFn    func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
	queryFn         func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error)
	putItemFn       func(*dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error)
	transactFn      func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error)
	describeTableFn func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error)

	getItemCalls  int
	transactCalls int
	// updateItemCalls counts UpdateItem calls (the Complete/Release mutation).
	updateItemCalls int
	// putItemCalls counts PutItem calls (the Persist mutation).
	putItemCalls int
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
	f.updateItemCalls++
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

func (f *fakeDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.mu.Lock()
	f.putItemCalls++
	fn := f.putItemFn
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDDB) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.mu.Lock()
	f.transactCalls++
	fn := f.transactFn
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func (f *fakeDDB) CreateTable(_ context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return &dynamodb.CreateTableOutput{}, nil
}

func (f *fakeDDB) DescribeTable(_ context.Context, in *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	f.mu.Lock()
	fn := f.describeTableFn
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
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
		metrics:            &ports.NoopExporter{},
		keys:               newKeyCache(defaultMaxKeyCache),
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

// Regression for J-N3: when the key cache misses and the RecordIDIndex GSI
// never converges within the bounded resolve retry, Complete must return a
// retryable (transient) error, NOT a permanent ErrNotFound. The record was
// just claimed (it exists in the base table), so the caller must retry
// Complete on GSI lag rather than treat it as absent and re-deliver.
func TestComplete_GSILagExhaustion_ReturnsRetryable(t *testing.T) {
	f := newFakeDDB()
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{}, nil // GSI lags on every attempt
	}
	s := newFakeStore(f) // resolveBackoff 0 → deterministic, no sleeping

	err := s.Complete(context.Background(), []string{"rec-cold"}, persistence.LeaseToken{Version: 1, Owner: "o"})
	if err == nil {
		t.Fatal("expected an error on GSI-lag exhaustion")
	}
	if errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("GSI-lag exhaustion must not return permanent ErrNotFound: %v", err)
	}
	if !errors.Is(err, shared.ErrTimeout) {
		t.Fatalf("expected retryable ErrTimeout on GSI-lag exhaustion, got %v", err)
	}
	if be, ok := shared.AsBridgeError(err); !ok || be.Class != shared.ErrorTransient {
		t.Fatalf("GSI-lag error must be classified transient (retryable), got %v", err)
	}
	// The bounded retry exhausted every configured GSI attempt before giving up.
	if got := f.queryCalls["RecordIDIndex"]; got != defaultResolveMaxAttempts {
		t.Fatalf("expected %d GSI attempts, got %d", defaultResolveMaxAttempts, got)
	}
}

// Regression for J-N1: record keys are cached on Claim and only evicted on
// terminal Complete, so records this instance claimed but never completes
// (lease churn) would grow the cache without bound. The LRU cap keeps it
// bounded and retains the hottest (most-recently-claimed) keys that a
// Complete is about to need.
func TestKeyCache_BoundedUnderClaimChurn(t *testing.T) {
	const capacity = 8
	s := newFakeStore(newFakeDDB())
	s.keys = newKeyCache(capacity)

	const churn = 1000
	for i := 0; i < churn; i++ {
		s.cacheKey(fmt.Sprintf("rec-%d", i), "PART#1", fmt.Sprintf("OUTBOX#e%d#b", i))
	}

	if got := s.keys.len(); got != capacity {
		t.Fatalf("key cache must stay bounded at %d under churn, got %d", capacity, got)
	}
	// The most-recently-claimed key is retained (hot); the oldest is evicted.
	if _, ok := s.lookupKey(fmt.Sprintf("rec-%d", churn-1)); !ok {
		t.Fatalf("most-recently-claimed key must be retained")
	}
	if _, ok := s.lookupKey("rec-0"); ok {
		t.Fatalf("oldest key must have been evicted under the cap")
	}
}

// --- transactional claim (fence TOCTOU) unit coverage ---

// pendingQueryItem builds a minimal base-table item a Claim query would
// return for a pending record.
func pendingQueryItem(pk, sk, recordID string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"PK":            &ddbtypes.AttributeValueMemberS{Value: pk},
		"SK":            &ddbtypes.AttributeValueMemberS{Value: sk},
		"record_id":     &ddbtypes.AttributeValueMemberS{Value: recordID},
		"envelope_id":   &ddbtypes.AttributeValueMemberS{Value: "env-1"},
		"binding_id":    &ddbtypes.AttributeValueMemberS{Value: "bind-1"},
		"session_id":    &ddbtypes.AttributeValueMemberS{Value: "sess-1"},
		"route_id":      &ddbtypes.AttributeValueMemberS{Value: "route-1"},
		"address":       &ddbtypes.AttributeValueMemberS{Value: "t/a"},
		"status":        &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
		"claim_version": &ddbtypes.AttributeValueMemberN{Value: "0"},
		"replay_count":  &ddbtypes.AttributeValueMemberN{Value: "0"},
		"created_at":    &ddbtypes.AttributeValueMemberN{Value: "1700000000000"},
		"seq":           &ddbtypes.AttributeValueMemberN{Value: "1"},
	}
}

// fenceGetItem returns a getItemFn serving a fence row at version v.
func fenceGetItem(v string) func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
	return func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
			attrMaxClaimVersion: &ddbtypes.AttributeValueMemberN{Value: v},
		}}, nil
	}
}

// dupRowGetItem models the row already occupying a queried SK as the SAME
// logical record — envelope_id/binding_id recovered from the SK itself — i.e. a
// genuine idempotent duplicate, the case Persist's verify-on-conflict collapses.
// conflictIsSameRecord issues this strongly-consistent readback when
// attribute_not_exists(SK) fails.
func dupRowGetItem() func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
	return func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		env, bind, _ := parseSortKey(strAttr(in.Key, "SK"))
		return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
			"envelope_id": &ddbtypes.AttributeValueMemberS{Value: env},
			"binding_id":  &ddbtypes.AttributeValueMemberS{Value: bind},
		}}, nil
	}
}

// transactCanceled fabricates the SDK error for a canceled claim
// transaction with the given per-item cancellation codes.
func transactCanceled(codes ...string) error {
	reasons := make([]ddbtypes.CancellationReason, len(codes))
	for i, c := range codes {
		code := c
		reasons[i] = ddbtypes.CancellationReason{Code: &code}
	}
	return &ddbtypes.TransactionCanceledException{CancellationReasons: reasons}
}

// Regression for the fence TOCTOU (finding: split-brain claims below a
// concurrently raised high-water-mark): when the transaction's fence
// ConditionCheck fails — a higher-version owner advanced the fence between
// our fence read and this claim — Claim must surface ErrStaleFencingToken,
// not keep claiming.
func TestClaim_FenceRaceDetectedByTransaction_ReturnsStaleToken(t *testing.T) {
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("5")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
			pendingQueryItem("PART#1", "OUTBOX#env-1#bind-1", "rec-1"),
		}}, nil
	}
	// Item 0 (fence check) failed: fence advanced past our version.
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return nil, transactCanceled("ConditionalCheckFailed", "None")
	}
	s := newFakeStore(f)

	_, err := s.Claim(context.Background(), "PART#1", persistence.LeaseToken{Version: 5, Owner: "a"}, 10)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("fence-check cancellation must map to ErrStaleFencingToken, got %v", err)
	}
	if f.transactCalls != 1 {
		t.Fatalf("expected exactly 1 claim transaction, got %d", f.transactCalls)
	}
}

// A record-level condition failure (another claimer won the record) is a
// benign skip: Claim returns no error and no record.
func TestClaim_RecordRaceLost_SkipsRecord(t *testing.T) {
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("5")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
			pendingQueryItem("PART#1", "OUTBOX#env-1#bind-1", "rec-1"),
		}}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return nil, transactCanceled("None", "ConditionalCheckFailed")
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), "PART#1", persistence.LeaseToken{Version: 5, Owner: "a"}, 10)
	if err != nil {
		t.Fatalf("record-race loss must not error: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("expected 0 claimed records, got %d", len(claimed))
	}
}

// FIX 4: a per-record claim aborted by a DynamoDB TransactionConflict is still
// a benign skip (no error, no record), but it must be COUNTED via
// shared.MetricOutboxClaimConflicts so a Claim under-filling because of contention is
// observable — and a plain record-level ConditionalCheckFailed (a normal lost
// race) must NOT be counted.
func TestClaim_TransactionConflict_CountsMetricAndSkips(t *testing.T) {
	t.Run("conflict is counted with partition tag", func(t *testing.T) {
		f := newFakeDDB()
		f.getItemFn = fenceGetItem("5")
		f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
				pendingQueryItem("PART#hot", "OUTBOX#env-1#bind-1", "rec-1"),
			}}, nil
		}
		// Fence check passed (item 0 "None"); the record update lost to a
		// concurrent writer with a transaction conflict (item 1).
		f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
			return nil, transactCanceled("None", "TransactionConflict")
		}
		rec := &ports.RecordingExporter{}
		s := newFakeStore(f)
		s.metrics = rec

		claimed, err := s.Claim(context.Background(), "PART#hot", persistence.LeaseToken{Version: 5, Owner: "a"}, 10)
		if err != nil {
			t.Fatalf("transaction conflict must not error: %v", err)
		}
		if len(claimed) != 0 {
			t.Fatalf("expected 0 claimed records on conflict, got %d", len(claimed))
		}

		entries := rec.FindEntries(shared.MetricOutboxClaimConflicts)
		if len(entries) != 1 {
			t.Fatalf("expected exactly 1 %s counter, got %d", shared.MetricOutboxClaimConflicts, len(entries))
		}
		if entries[0].Kind != "counter" || entries[0].IValue != 1 {
			t.Fatalf("conflict metric must be a counter incremented by 1, got %+v", entries[0])
		}
		var tagged bool
		for _, tag := range entries[0].Tags {
			if tag.Key == shared.TagKeyPartition && tag.Value == "PART#hot" {
				tagged = true
			}
		}
		if !tagged {
			t.Fatalf("conflict metric must carry the partition tag %q=PART#hot, tags=%v",
				shared.TagKeyPartition, entries[0].Tags)
		}
	})

	t.Run("record-level condition failure is NOT counted", func(t *testing.T) {
		f := newFakeDDB()
		f.getItemFn = fenceGetItem("5")
		f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
				pendingQueryItem("PART#1", "OUTBOX#env-1#bind-1", "rec-1"),
			}}, nil
		}
		f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
			return nil, transactCanceled("None", "ConditionalCheckFailed")
		}
		rec := &ports.RecordingExporter{}
		s := newFakeStore(f)
		s.metrics = rec

		if _, err := s.Claim(context.Background(), "PART#1", persistence.LeaseToken{Version: 5, Owner: "a"}, 10); err != nil {
			t.Fatalf("record-race loss must not error: %v", err)
		}
		if got := len(rec.FindEntries(shared.MetricOutboxClaimConflicts)); got != 0 {
			t.Fatalf("a benign record-level lost race must not emit %s, got %d", shared.MetricOutboxClaimConflicts, got)
		}
	})

	// The DynamoDBStoreFactory installs the runtime exporter by appending the
	// PUBLIC WithMetrics option (not by touching the unexported field). Exercise
	// that exact option path — apply WithMetrics(rec) as NewStore would — and
	// assert a conflict is counted, so a regression in the option wiring is
	// caught without a live DynamoDB.
	t.Run("WithMetrics option (factory path) routes the conflict counter", func(t *testing.T) {
		f := newFakeDDB()
		f.getItemFn = fenceGetItem("5")
		f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
				pendingQueryItem("PART#opt", "OUTBOX#env-1#bind-1", "rec-1"),
			}}, nil
		}
		f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
			return nil, transactCanceled("None", "TransactionConflict")
		}
		rec := &ports.RecordingExporter{}
		s := newFakeStore(f)
		// Route through the exported option, exactly as the factory does via
		// dynamodboutbox.NewStore(client, WithMetrics(runtime.Metrics)).
		WithMetrics(rec)(s)

		if _, err := s.Claim(context.Background(), "PART#opt", persistence.LeaseToken{Version: 5, Owner: "a"}, 10); err != nil {
			t.Fatalf("transaction conflict must not error: %v", err)
		}
		if got := len(rec.FindEntries(shared.MetricOutboxClaimConflicts)); got != 1 {
			t.Fatalf("WithMetrics option must route the conflict counter to the exporter, got %d", got)
		}
	})
}

// TestClaim_ThrottleCancellation_SurfacesRetryable is the c13-txn-throttle
// regression. A claim TransactWriteItems canceled for a reason OTHER than the
// fence ConditionalCheckFailed — a throttle (ProvisionedThroughputExceeded /
// ThrottlingError) or a permanent fault (ValidationError) — must NOT be
// swallowed as a benign (nil, nil) skip. Swallowing it once dropped the record
// with no backoff signal to the drainer, so a throttled partition self-
// throttled harder and validation faults hid forever. The claim must surface a
// classified error: a retryable shared.ErrThrottled so the drainer BACKS OFF,
// and a permanent shared.ErrInvalidPayload that is never retried into oblivion.
//
// Mutation this kills: collapsing every non-fence cancellation reason back to
// (nil, nil) → err becomes nil → this test FAILs.
func TestClaim_ThrottleCancellation_SurfacesRetryable(t *testing.T) {
	tests := []struct {
		name          string
		codes         []string
		wantSentinel  error
		wantTransient bool
	}{
		{"throughput on the record update", []string{ccReasonNone, "ProvisionedThroughputExceeded"}, shared.ErrThrottled, true},
		{"throttling on the fence check", []string{"ThrottlingError", ccReasonNone}, shared.ErrThrottled, true},
		{"validation is permanent", []string{ccReasonNone, "ValidationError"}, shared.ErrInvalidPayload, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDDB()
			f.getItemFn = fenceGetItem("0")
			f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
					pendingQueryItem("PART#throttle", "OUTBOX#env-1#bind-1", "rec-1"),
				}}, nil
			}
			f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
				return nil, transactCanceled(tc.codes...)
			}
			s := newFakeStore(f)

			claimed, err := s.Claim(context.Background(), "PART#throttle",
				persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
			if err == nil {
				t.Fatalf("a %v cancellation must surface an error, not a silent skip (got claimed=%v)", tc.codes, claimed)
			}
			if claimed != nil {
				t.Fatalf("no records may be returned alongside the error, got %v", claimed)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Fatalf("cancellation %v must classify as %v, got %v", tc.codes, tc.wantSentinel, err)
			}
			be, ok := shared.AsBridgeError(err)
			if !ok {
				t.Fatalf("expected a *shared.BridgeError, got %T", err)
			}
			if transient := be.Class == shared.ErrorTransient; transient != tc.wantTransient {
				t.Fatalf("cancellation %v transient=%v, want %v (class=%s)", tc.codes, transient, tc.wantTransient, be.Class)
			}
		})
	}
}

// A pure fence-conflict (item 0 ConditionalCheckFailed) remains benign
// contention: it surfaces ErrStaleFencingToken, NOT a throttle/permanent
// error, so the c13-txn-throttle fix does not over-classify a legitimate
// preemption. (The full fence-TOCTOU semantics are pinned separately by
// TestClaim_FenceRaceDetectedByTransaction_ReturnsStaleToken.)
func TestClaim_FenceConflict_StaysBenignAfterThrottleFix(t *testing.T) {
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("5")
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
			pendingQueryItem("PART#1", "OUTBOX#env-1#bind-1", "rec-1"),
		}}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return nil, transactCanceled(ccReasonCondCheckFailed, ccReasonNone)
	}
	s := newFakeStore(f)

	_, err := s.Claim(context.Background(), "PART#1", persistence.LeaseToken{Version: 5, Owner: "a"}, 10)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("fence conflict must stay ErrStaleFencingToken, got %v", err)
	}
	if errors.Is(err, shared.ErrThrottled) || errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatalf("fence conflict must not be reclassified as a fault: %v", err)
	}
}

// Every per-record claim must pair the record update with a ConditionCheck
// on the partition FENCE row inside one TransactWriteItems — the shape that
// closes the check-then-claim TOCTOU.
func TestClaim_TransactionPairsFenceCheckWithRecordUpdate(t *testing.T) {
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("5")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
			pendingQueryItem("PART#1", "OUTBOX#env-1#bind-1", "rec-1"),
		}}, nil
	}
	var captured *dynamodb.TransactWriteItemsInput
	f.transactFn = func(in *dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		captured = in
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), "PART#1", persistence.LeaseToken{Version: 5, Owner: "a"}, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID() != "rec-1" {
		t.Fatalf("expected rec-1 claimed, got %v", claimed)
	}
	if claimed[0].ClaimVersion() != 5 || claimed[0].ClaimedBy() != "a" {
		t.Fatalf("synthesized claim state wrong: ver=%d owner=%q", claimed[0].ClaimVersion(), claimed[0].ClaimedBy())
	}
	if claimed[0].ReplayCount() != 1 {
		t.Fatalf("synthesized replay count: got %d, want 1", claimed[0].ReplayCount())
	}

	if captured == nil || len(captured.TransactItems) != 2 {
		t.Fatalf("claim must be a 2-item transaction, got %+v", captured)
	}
	check := captured.TransactItems[0].ConditionCheck
	if check == nil || strAttr(check.Key, "SK") != fenceSK {
		t.Fatalf("item 0 must be a ConditionCheck on the FENCE row, got %+v", captured.TransactItems[0])
	}
	upd := captured.TransactItems[1].Update
	if upd == nil || strAttr(upd.Key, "SK") != "OUTBOX#env-1#bind-1" {
		t.Fatalf("item 1 must be the record update, got %+v", captured.TransactItems[1])
	}
}

// Regression for the poison-record deadlock (finding 1 / contract C2): the
// store must never filter claimable records by replay count — poison
// detection is the drainer's decision. A record whose replay_count is far
// past any poison threshold must still be claimable so the drainer can
// route it to the DLQ.
func TestClaim_NoReplayCountFilter_HighReplayRecordStillClaimable(t *testing.T) {
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("5")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.FilterExpression != nil && strings.Contains(*in.FilterExpression, "replay_count") {
			t.Fatalf("claim query must not filter on replay_count: %s", *in.FilterExpression)
		}
		item := pendingQueryItem("PART#1", "OUTBOX#env-1#bind-1", "rec-1")
		item["replay_count"] = &ddbtypes.AttributeValueMemberN{Value: "97"}
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{item}}, nil
	}
	f.transactFn = func(in *dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		upd := in.TransactItems[1].Update
		if upd.ConditionExpression != nil && strings.Contains(*upd.ConditionExpression, "replay_count <") {
			t.Fatalf("claim condition must not gate on replay_count: %s", *upd.ConditionExpression)
		}
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), "PART#1", persistence.LeaseToken{Version: 5, Owner: "a"}, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("high-replay record must be claimable, got %d records", len(claimed))
	}
	if claimed[0].ReplayCount() != 98 {
		t.Fatalf("replay count: got %d, want 98", claimed[0].ReplayCount())
	}
}

// Claim with limit <= 0 is a fencing no-op (ports.OutboxStore contract):
// the fence is validated and raised, no partition scan happens and no
// record transaction runs.
func TestClaim_ZeroLimit_FenceOnlyNoScan(t *testing.T) {
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("3")
	var fenceRaised bool
	f.updateItemFn = func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		if strAttr(in.Key, "SK") == fenceSK {
			fenceRaised = true
		}
		return &dynamodb.UpdateItemOutput{}, nil
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), "PART#1", persistence.LeaseToken{Version: 7, Owner: "a"}, 0)
	if err != nil {
		t.Fatalf("zero-limit claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("zero-limit claim must return no records, got %d", len(claimed))
	}
	if !fenceRaised {
		t.Fatalf("zero-limit claim must still raise the fence high-water-mark")
	}
	if got := f.queryCalls[""]; got != 0 {
		t.Fatalf("zero-limit claim must not scan the partition, got %d queries", got)
	}
	if f.transactCalls != 0 {
		t.Fatalf("zero-limit claim must not run record transactions, got %d", f.transactCalls)
	}
}

// TestClaim_IndexFastPath_StopsAtLimitWithoutPagingWholePartition is the
// c13-claim-quadratic regression. Claim's fast path queries the age-ordered
// ClaimIndex GSI oldest-first and must STOP as soon as `limit` records are
// claimed, even when the partition holds a far deeper backlog (more index
// pages exist). The pre-fix Claim paged the WHOLE partition every batch to
// find the oldest-N, going O(backlog) and self-throttling DynamoDB after an
// outage. Here the first index page already yields `limit` claimable records
// AND signals more via a non-nil LastEvaluatedKey; the fast path must claim
// `limit` and issue exactly ONE index query, never touching the base-table
// full-partition scan.
//
// Mutation this kills: removing the early stop (changing the paging loop
// `for len(claimed) < limit` to page to LastEvaluatedKey exhaustion) makes
// Claim fetch page 2 → f.queryCalls[ClaimIndex] becomes 2 → this test FAILs.
func TestClaim_IndexFastPath_StopsAtLimitWithoutPagingWholePartition(t *testing.T) {
	const (
		partition = "PART#backlog"
		limit     = 3
	)
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")

	page := 0
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName == nil || *in.IndexName != claimIndexName {
			t.Fatalf("fast path must query the %s GSI, got index=%v", claimIndexName, in.IndexName)
		}
		page++
		if page == 1 {
			items := make([]map[string]ddbtypes.AttributeValue, limit)
			for i := 0; i < limit; i++ {
				items[i] = pendingQueryItem(partition,
					fmt.Sprintf("OUTBOX#env-%d#bind", i), fmt.Sprintf("rec-%d", i))
			}
			// A full page of `limit` claimable records WITH more pages behind
			// it: the fast path must not fetch the next page.
			return &dynamodb.QueryOutput{
				Items: items,
				LastEvaluatedKey: map[string]ddbtypes.AttributeValue{
					"PK":          &ddbtypes.AttributeValueMemberS{Value: partition},
					attrClaimSort: &ddbtypes.AttributeValueMemberS{Value: "cursor"},
				},
			}, nil
		}
		// Page 2+ exists ONLY so the page-to-exhaustion mutant terminates; a
		// correct Claim never asks for it.
		return &dynamodb.QueryOutput{}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	s := newFakeStore(f)

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, limit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != limit {
		t.Fatalf("expected %d claimed, got %d", limit, len(claimed))
	}
	// The heart of the fix: exactly ONE index page even though more pages
	// exist, and the base-table full-partition scan is never touched.
	if got := f.queryCalls[claimIndexName]; got != 1 {
		t.Fatalf("Claim must stop after one index page once `limit` are claimed, got %d pages", got)
	}
	if got := f.queryCalls[""]; got != 0 {
		t.Fatalf("fast path must not scan the base table, got %d scans", got)
	}
}

// TestClaim_MissingIndex_FallsBackToScanAndWarnsOnce pins the backward-compat
// guarantee: a table created before the ClaimIndex GSI existed still works.
// The first Claim probes the GSI, DynamoDB rejects it (missing index), and
// Claim must LATCH the absence, WARN exactly once, and fall back to the
// always-correct base-table scan — still claiming the record. A subsequent
// Claim goes straight to the scan without re-probing the GSI.
func TestClaim_MissingIndex_FallsBackToScanAndWarnsOnce(t *testing.T) {
	const partition = "PART#unmigrated"
	f := newFakeDDB()
	f.getItemFn = fenceGetItem("0")
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		if in.IndexName != nil && *in.IndexName == claimIndexName {
			// An un-migrated table lacks ClaimIndex: DynamoDB rejects the query
			// with a ValidationException naming the missing index.
			return nil, errors.New("ValidationException: The table does not have the specified index: ClaimIndex")
		}
		return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
			pendingQueryItem(partition, "OUTBOX#env-1#bind-1", "rec-1"),
		}}, nil
	}
	f.transactFn = func(*dynamodb.TransactWriteItemsInput) (*dynamodb.TransactWriteItemsOutput, error) {
		return &dynamodb.TransactWriteItemsOutput{}, nil
	}
	logger, buf := warnBufLogger()
	s := newFakeStore(f)
	s.logger = logger

	claimed, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10)
	if err != nil {
		t.Fatalf("missing-index claim must fall back, not error: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("scan fallback must still claim the record, got %d", len(claimed))
	}
	if !s.claimIndexAbsent.Load() {
		t.Fatalf("a missing ClaimIndex must be latched so later Claims skip the GSI probe")
	}
	if got := f.queryCalls[""]; got == 0 {
		t.Fatalf("Claim must fall back to the base-table scan when the GSI is absent")
	}

	// Second Claim: absence is latched, so it must go straight to the scan and
	// NOT re-probe the GSI; the WARN must stay at exactly one.
	gsiProbes := f.queryCalls[claimIndexName]
	if _, err := s.Claim(context.Background(), partition,
		persistence.LeaseToken{Version: 1, Owner: "drainer"}, 10); err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if f.queryCalls[claimIndexName] != gsiProbes {
		t.Fatalf("latched absence must skip further GSI probes, got %d then %d",
			gsiProbes, f.queryCalls[claimIndexName])
	}
	if n := strings.Count(buf.String(), "ClaimIndex GSI unusable"); n != 1 {
		t.Fatalf("expected exactly ONE ClaimIndex-unusable WARN, got %d in: %q", n, buf.String())
	}
}

// --- Persist (per-record idempotency + seq allocation) unit coverage ---

// Persist allocates monotonic per-partition seqs from the FENCE row's
// atomic counter and stamps each written item; duplicate identities are
// skipped per-record, and ErrDuplicateRecord surfaces only when the whole
// batch already existed.
func TestPersist_AllocatesSeqsAndSkipsDuplicates(t *testing.T) {
	f := newFakeDDB()
	var counter int64
	f.updateItemFn = func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		if strAttr(in.Key, "SK") != fenceSK {
			t.Fatalf("persist must only update the FENCE row, got SK %q", strAttr(in.Key, "SK"))
		}
		n, _ := strconv.ParseInt((in.ExpressionAttributeValues[":n"].(*ddbtypes.AttributeValueMemberN)).Value, 10, 64)
		counter += n
		return &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{
			attrSeqCounter: &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(counter, 10)},
		}}, nil
	}
	var seqs []string
	putCalls := 0
	f.getItemFn = dupRowGetItem()
	f.putItemFn = func(in *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		putCalls++
		if in.ConditionExpression == nil || *in.ConditionExpression != "attribute_not_exists(SK)" {
			t.Fatalf("persist put must be conditional on attribute_not_exists(SK)")
		}
		if n, ok := in.Item["seq"].(*ddbtypes.AttributeValueMemberN); ok {
			seqs = append(seqs, n.Value)
		}
		if putCalls == 2 {
			// Second record already exists: per-record skip, not an error.
			return nil, &ddbtypes.ConditionalCheckFailedException{}
		}
		return &dynamodb.PutItemOutput{}, nil
	}
	s := newFakeStore(f)

	records := []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "p-1", RouteID: "r", EnvelopeID: "e1", BindingID: "b1", SessionID: "s1", Address: "a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "t"}),
		}),
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "p-2", RouteID: "r", EnvelopeID: "e2", BindingID: "b1", SessionID: "s1", Address: "a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e2", Subject: "t"}),
		}),
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "p-3", RouteID: "r", EnvelopeID: "e3", BindingID: "b1", SessionID: "s1", Address: "a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e3", Subject: "t"}),
		}),
	}
	if err := s.Persist(context.Background(), records); err != nil {
		t.Fatalf("persist with one duplicate must succeed: %v", err)
	}
	if putCalls != 3 {
		t.Fatalf("expected 3 conditional puts, got %d", putCalls)
	}
	want := []string{"1", "2", "3"}
	for i, w := range want {
		if seqs[i] != w {
			t.Fatalf("seq[%d]: got %s, want %s (all seqs %v)", i, seqs[i], w, seqs)
		}
	}
}

// An all-duplicate batch returns ErrDuplicateRecord (the whole batch was a
// replay); nothing else is treated as an error.
func TestPersist_AllDuplicates_ReturnsErrDuplicateRecord(t *testing.T) {
	f := newFakeDDB()
	f.updateItemFn = func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		return &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{
			attrSeqCounter: &ddbtypes.AttributeValueMemberN{Value: "7"},
		}}, nil
	}
	f.putItemFn = func(in *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	f.getItemFn = dupRowGetItem()
	s := newFakeStore(f)

	records := []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "pd-1", RouteID: "r", EnvelopeID: "e1", BindingID: "b1", SessionID: "s1", Address: "a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "t"}),
		}),
	}
	err := s.Persist(context.Background(), records)
	if !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("all-duplicate batch must return ErrDuplicateRecord, got %v", err)
	}
}

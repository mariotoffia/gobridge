package dynamodboutbox

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// TestCountPending_BoundedCount_SinglePage exercises the happy path: a
// Select=COUNT Query against the sparse per-partition ClaimIndex returns the
// pending count in one page. The result is the reported depth, and the base
// table is never scanned.
func TestCountPending_BoundedCount_SinglePage(t *testing.T) {
	f := newFakeDDB()
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		// The count MUST be served by the bounded ClaimIndex COUNT query.
		if in.IndexName == nil || *in.IndexName != claimIndexName {
			t.Fatalf("CountPending must query the %s GSI, got index %v", claimIndexName, in.IndexName)
		}
		if in.Select != ddbtypes.SelectCount {
			t.Fatalf("CountPending must use Select=COUNT (no record materialisation), got %v", in.Select)
		}
		if in.FilterExpression == nil || *in.FilterExpression != "#st = :pending" {
			t.Fatalf("CountPending must filter on pending status, got %v", in.FilterExpression)
		}
		if v, ok := in.ExpressionAttributeValues[":pending"].(*ddbtypes.AttributeValueMemberS); !ok ||
			v.Value != string(persistence.OutboxPending) {
			t.Fatalf("CountPending pending filter value = %v; want %q", in.ExpressionAttributeValues[":pending"], persistence.OutboxPending)
		}
		return &dynamodb.QueryOutput{Count: 7}, nil
	}
	s := newFakeStore(f)

	n, err := s.CountPending(context.Background(), "SESSION#s1")
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 7 {
		t.Fatalf("CountPending = %d; want 7", n)
	}
	if got := f.queryCalls[""]; got != 0 {
		t.Fatalf("CountPending must not scan the base table; base-table queries = %d", got)
	}
	if got := f.queryCalls[claimIndexName]; got != 1 {
		t.Fatalf("CountPending ClaimIndex queries = %d; want 1", got)
	}
}

// TestCountPending_BoundedCount_Paginated proves the per-page Count is summed
// across DynamoDB pagination (COUNT returns Count per page, not a grand total).
func TestCountPending_BoundedCount_Paginated(t *testing.T) {
	f := newFakeDDB()
	page := 0
	f.queryFn = func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		page++
		switch page {
		case 1:
			if in.ExclusiveStartKey != nil {
				t.Fatalf("first page must not carry an ExclusiveStartKey")
			}
			return &dynamodb.QueryOutput{
				Count: 5,
				LastEvaluatedKey: map[string]ddbtypes.AttributeValue{
					"PK": &ddbtypes.AttributeValueMemberS{Value: "SESSION#s1"},
				},
			}, nil
		case 2:
			if in.ExclusiveStartKey == nil {
				t.Fatalf("second page must resume from the first page's LastEvaluatedKey")
			}
			return &dynamodb.QueryOutput{Count: 3}, nil
		default:
			t.Fatalf("unexpected page %d", page)
			return nil, nil
		}
	}
	s := newFakeStore(f)

	n, err := s.CountPending(context.Background(), "SESSION#s1")
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 8 {
		t.Fatalf("CountPending = %d; want 8 (5+3 across pages)", n)
	}
	if got := f.queryCalls[claimIndexName]; got != 2 {
		t.Fatalf("CountPending ClaimIndex queries = %d; want 2 (paginated)", got)
	}
}

// TestCountPending_AllPartitions_Unsupported documents the fleet-wide path:
// partitionKey == "" has no bounded DynamoDB access path (the ClaimIndex is
// hashed on PK), so CountPending returns ports.ErrOutboxDepthUnsupported and
// issues NO query — the drainer falls back to the saturating claimed-count.
func TestCountPending_AllPartitions_Unsupported(t *testing.T) {
	f := newFakeDDB()
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		t.Fatalf("fleet-wide CountPending must not issue any query")
		return nil, nil
	}
	s := newFakeStore(f)

	n, err := s.CountPending(context.Background(), "")
	if !errors.Is(err, ports.ErrOutboxDepthUnsupported) {
		t.Fatalf("CountPending(\"\"): got err %v; want ports.ErrOutboxDepthUnsupported", err)
	}
	if n != 0 {
		t.Fatalf("CountPending(\"\") = %d; want 0", n)
	}
}

// TestCountPending_IndexAlreadyLatched_Unsupported: once the Claim fast path has
// latched the ClaimIndex unusable, a bounded count is impossible, so CountPending
// READS that shared latch and reports the capability unavailable without issuing
// any query — rather than falling back to a full-partition scan. (CountPending
// only READS the latch here; it never WRITES it — see
// TestCountPending_MissingIndex_DoesNotLatchClaimPath.)
func TestCountPending_IndexAlreadyLatched_Unsupported(t *testing.T) {
	f := newFakeDDB()
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		t.Fatalf("CountPending must not query when the ClaimIndex is latched unusable")
		return nil, nil
	}
	s := newFakeStore(f)
	s.claimIndexAbsent.Store(true)

	n, err := s.CountPending(context.Background(), "SESSION#s1")
	if !errors.Is(err, ports.ErrOutboxDepthUnsupported) {
		t.Fatalf("CountPending(latched): got err %v; want ports.ErrOutboxDepthUnsupported", err)
	}
	if n != 0 {
		t.Fatalf("CountPending(latched) = %d; want 0", n)
	}
	// Reading the latch must leave it set (CountPending neither clears nor
	// depends on clearing it).
	if !s.claimIndexAbsent.Load() {
		t.Fatalf("CountPending must not clear a Claim-set latch")
	}
}

// TestCountPending_MissingIndex_DegradesToUnsupported: an un-migrated /
// misprojected table surfaces a missing-index ValidationException on the COUNT
// query. CountPending classifies it (via the shared claimIndexUnusableReason
// helper) and returns ports.ErrOutboxDepthUnsupported — a benign "cannot report
// depth", NOT a real fault — so the drainer keeps its saturating fallback.
func TestCountPending_MissingIndex_DegradesToUnsupported(t *testing.T) {
	f := newFakeDDB()
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return nil, errors.New("ValidationException: The table does not have the specified index: ClaimIndex")
	}
	s := newFakeStore(f)

	n, err := s.CountPending(context.Background(), "SESSION#s1")
	if !errors.Is(err, ports.ErrOutboxDepthUnsupported) {
		t.Fatalf("CountPending(missing index): got err %v; want ports.ErrOutboxDepthUnsupported", err)
	}
	if n != 0 {
		t.Fatalf("CountPending(missing index) = %d; want 0", n)
	}
}

// TestCountPending_MissingIndex_DoesNotLatchClaimPath is the cross-path
// side-effect regression: CountPending is a READ-ONLY depth reporter, so a
// depth/metrics probe that hits a missing/mis-projected ClaimIndex must NOT
// mutate the shared claimIndexAbsent latch that the Claim fast path reads.
// Otherwise a single depth cycle would silently force EVERY subsequent Claim in
// this process onto the exhaustive base-table scan — a read degrading the write
// path. The latch must be observably UNCHANGED across the CountPending call.
//
// Mutation-verify: re-introduce s.markClaimIndexUnusable(...) in CountPending's
// unusable-index branch and this test FAILS; remove it and it PASSES.
func TestCountPending_MissingIndex_DoesNotLatchClaimPath(t *testing.T) {
	f := newFakeDDB()
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return nil, errors.New("ValidationException: The table does not have the specified index: ClaimIndex")
	}
	s := newFakeStore(f)

	if s.claimIndexAbsent.Load() {
		t.Fatalf("precondition: Claim latch must start unset")
	}

	_, err := s.CountPending(context.Background(), "SESSION#s1")
	if !errors.Is(err, ports.ErrOutboxDepthUnsupported) {
		t.Fatalf("CountPending(missing index): got err %v; want ports.ErrOutboxDepthUnsupported", err)
	}

	if s.claimIndexAbsent.Load() {
		t.Fatalf("CountPending latched claimIndexAbsent on a missing index; a read-only depth probe must NOT degrade the Claim write path")
	}
}

// TestCountPending_ContextCancelled: a cancelled context short-circuits the
// count loop before any Query, so a count never outlives its context (the
// drainer's per-cycle deadline bounds it).
func TestCountPending_ContextCancelled(t *testing.T) {
	f := newFakeDDB()
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		t.Fatalf("CountPending must not query once the context is cancelled")
		return nil, nil
	}
	s := newFakeStore(f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := s.CountPending(ctx, "SESSION#s1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CountPending(cancelled): got err %v; want context.Canceled", err)
	}
	if n != 0 {
		t.Fatalf("CountPending(cancelled) = %d; want 0", n)
	}
}

// TestCountPending_RealError_Returned: a genuine backend failure (throttling,
// server error) is returned AS-IS — NOT masked behind
// ports.ErrOutboxDepthUnsupported — so the drainer treats it as a real
// depth-query failure (skip emission + MetricOutboxDepthFailures), never as the
// benign fallback.
func TestCountPending_RealError_Returned(t *testing.T) {
	f := newFakeDDB()
	sentinel := errors.New("ProvisionedThroughputExceededException: throttled")
	f.queryFn = func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return nil, sentinel
	}
	s := newFakeStore(f)

	n, err := s.CountPending(context.Background(), "SESSION#s1")
	if err == nil {
		t.Fatal("CountPending: expected a real backend error, got nil")
	}
	if errors.Is(err, ports.ErrOutboxDepthUnsupported) {
		t.Fatalf("a real backend error must NOT be reported as ErrOutboxDepthUnsupported, got %v", err)
	}
	if n != 0 {
		t.Fatalf("CountPending on error = %d; want 0", n)
	}
	if s.claimIndexAbsent.Load() {
		t.Fatalf("a real (non-index) error must not latch the ClaimIndex as unusable")
	}
}

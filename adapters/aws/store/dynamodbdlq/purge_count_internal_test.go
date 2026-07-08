package dynamodbdlq

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TestPurge_ExactCount_IgnoresAlreadyDeleted is the MINOR regression: Purge's
// reported count must reflect rows ACTUALLY removed, not the number of
// DeleteItem calls issued. It requests ReturnValues ALL_OLD and counts only
// deletes that echoed an item, so an idempotent no-op delete of an
// already-removed key (phantom / scan overlap / concurrent purge) does not
// inflate the total.
//
// Counterfactual: the pre-fix Purge issued DeleteItem with no ReturnValues and
// did count++ unconditionally — it would fail the ALL_OLD assertion and report
// 2 for the single real removal below.
func TestPurge_ExactCount_IgnoresAlreadyDeleted(t *testing.T) {
	f := &fakeDLQClient{}
	item := func(id string) map[string]ddbtypes.AttributeValue {
		return map[string]ddbtypes.AttributeValue{
			attrPK:       &ddbtypes.AttributeValueMemberS{Value: dlqKey(id)},
			attrFailedAt: &ddbtypes.AttributeValueMemberN{Value: i64(time.Unix(1_700_000_000, 0).UnixMilli())},
		}
	}
	// A single scan page surfaces the SAME pk twice (overlap/phantom): the
	// second DeleteItem is an idempotent no-op that must not be counted.
	f.scanFn = func(call int, _ *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		if call == 1 {
			return &dynamodb.ScanOutput{Items: []map[string]ddbtypes.AttributeValue{item("p1"), item("p1")}}, nil
		}
		return &dynamodb.ScanOutput{}, nil
	}
	deleted := map[string]bool{}
	f.deleteFn = func(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
		if in.ReturnValues != ddbtypes.ReturnValueAllOld {
			t.Fatalf("Purge must request ReturnValues ALL_OLD for an exact count, got %q", in.ReturnValues)
		}
		id := strAttr(in.Key, attrPK)
		if deleted[id] {
			return &dynamodb.DeleteItemOutput{}, nil // already removed: no echo
		}
		deleted[id] = true
		return &dynamodb.DeleteItemOutput{Attributes: map[string]ddbtypes.AttributeValue{
			attrPK: &ddbtypes.AttributeValueMemberS{Value: id},
		}}, nil
	}
	s := newDLQStore(f)

	n, err := s.Purge(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("exact count must ignore the already-deleted phantom: got %d, want 1", n)
	}
	if f.deleteCalls != 2 {
		t.Fatalf("expected 2 DeleteItem calls (1 real + 1 phantom), got %d", f.deleteCalls)
	}
}

package dynamodblease

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

const attrTTLName = "ttl"

// captureClient is an in-memory dynamoAPI seam that records the requests
// issued by the lease store so unit tests can assert on the exact item
// shape written to DynamoDB without a live backend.
type captureClient struct {
	putItems    []*dynamodb.PutItemInput
	updateItems []*dynamodb.UpdateItemInput
}

func (c *captureClient) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	c.putItems = append(c.putItems, in)
	return &dynamodb.PutItemOutput{}, nil
}

func (c *captureClient) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	c.updateItems = append(c.updateItems, in)
	return &dynamodb.UpdateItemOutput{
		Attributes: map[string]ddbtypes.AttributeValue{
			attrVersion: &ddbtypes.AttributeValueMemberN{Value: "2"},
		},
	}, nil
}

func (c *captureClient) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, nil
}

func (c *captureClient) CreateTable(_ context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return &dynamodb.CreateTableOutput{}, nil
}

func (c *captureClient) DescribeTable(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}

func (c *captureClient) DescribeTimeToLive(_ context.Context, _ *dynamodb.DescribeTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error) {
	return &dynamodb.DescribeTimeToLiveOutput{}, nil
}

func newCaptureStore(c *captureClient) *Store {
	return &Store{client: c, tableName: "leases-test", clk: clock.System}
}

// Regression for J1: fencing-counter rows must never carry a TTL
// attribute, otherwise DynamoDB TTL deletion of a released lease resets
// the version counter to 1 and breaks fencing-token monotonicity.
func TestLeaseWrites_CarryNoTTLAttribute(t *testing.T) {
	ctx := context.Background()

	t.Run("Acquire_fresh_PutItem_has_no_ttl", func(t *testing.T) {
		c := &captureClient{}
		s := newCaptureStore(c)

		if _, err := s.Acquire(ctx, "l1", "owner-a", time.Minute, nil); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if len(c.putItems) != 1 {
			t.Fatalf("expected 1 PutItem, got %d", len(c.putItems))
		}
		if _, ok := c.putItems[0].Item[attrTTLName]; ok {
			t.Fatalf("fresh acquire wrote a %q attribute; lease rows must never be TTL-deleted", attrTTLName)
		}
	})

	t.Run("Renew_update_strips_ttl_and_assigns_none", func(t *testing.T) {
		c := &captureClient{}
		s := newCaptureStore(c)

		tok := persistence.LeaseToken{Version: 1, Owner: "owner-a"}
		if _, err := s.Renew(ctx, "l1", tok, time.Minute, nil); err != nil {
			t.Fatalf("renew: %v", err)
		}
		assertStripsTTL(t, c)
	})

	t.Run("Release_update_strips_ttl_and_assigns_none", func(t *testing.T) {
		c := &captureClient{}
		s := newCaptureStore(c)

		tok := persistence.LeaseToken{Version: 1, Owner: "owner-a"}
		if err := s.Release(ctx, "l1", tok); err != nil {
			t.Fatalf("release: %v", err)
		}
		assertStripsTTL(t, c)
	})
}

// assertStripsTTL verifies the single captured UpdateItem sheds any legacy
// ttl (REMOVE #ttl mapped to "ttl") and never *assigns* a ttl in its SET
// clause.
func assertStripsTTL(t *testing.T, c *captureClient) {
	t.Helper()
	if len(c.updateItems) != 1 {
		t.Fatalf("expected 1 UpdateItem, got %d", len(c.updateItems))
	}
	up := c.updateItems[0]
	if up.UpdateExpression == nil {
		t.Fatal("nil UpdateExpression")
	}
	expr := *up.UpdateExpression
	if !strings.Contains(expr, "REMOVE #ttl") {
		t.Fatalf("update expression does not strip legacy ttl (missing REMOVE #ttl): %q", expr)
	}
	if up.ExpressionAttributeNames["#ttl"] != attrTTLName {
		t.Fatalf("#ttl must map to %q, names=%v", attrTTLName, up.ExpressionAttributeNames)
	}
	// The SET portion must not assign ttl (that would re-introduce the hazard).
	setPart := expr
	if i := strings.Index(expr, "REMOVE"); i >= 0 {
		setPart = expr[:i]
	}
	if strings.Contains(setPart, "#ttl") {
		t.Fatalf("SET clause assigns ttl (must only REMOVE it): %q", expr)
	}
}

// mutatingClient is a small stateful dynamoAPI fake that models the DynamoDB
// REMOVE clause so tests can seed a row carrying a stale legacy `ttl` and
// prove the store strips it. Only the semantics exercised by the lease store
// are modelled.
type mutatingClient struct {
	item map[string]ddbtypes.AttributeValue // current row, nil when absent
}

func (m *mutatingClient) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	// Model attribute_not_exists(#pk): a fresh acquire fails when a row exists.
	if m.item != nil && in.ConditionExpression != nil &&
		strings.Contains(*in.ConditionExpression, "attribute_not_exists") {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	m.item = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (m *mutatingClient) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if m.item == nil {
		m.item = map[string]ddbtypes.AttributeValue{}
	}
	applyRemove(m.item, in)
	// Ensure a version attribute exists for takeover-result parsing.
	if _, ok := m.item[attrVersion]; !ok {
		m.item[attrVersion] = &ddbtypes.AttributeValueMemberN{Value: "2"}
	}
	return &dynamodb.UpdateItemOutput{Attributes: m.item}, nil
}

func (m *mutatingClient) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{Item: m.item}, nil
}

func (m *mutatingClient) CreateTable(_ context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return &dynamodb.CreateTableOutput{}, nil
}

func (m *mutatingClient) DescribeTable(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}

func (m *mutatingClient) DescribeTimeToLive(_ context.Context, _ *dynamodb.DescribeTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error) {
	return &dynamodb.DescribeTimeToLiveOutput{}, nil
}

// applyRemove models the DynamoDB `REMOVE #a, #b` clause: it resolves each
// listed name via ExpressionAttributeNames and deletes it from the item.
func applyRemove(item map[string]ddbtypes.AttributeValue, in *dynamodb.UpdateItemInput) {
	if in.UpdateExpression == nil {
		return
	}
	expr := *in.UpdateExpression
	i := strings.Index(expr, "REMOVE")
	if i < 0 {
		return
	}
	for _, tok := range strings.Split(expr[i+len("REMOVE"):], ",") {
		name := strings.TrimSpace(tok)
		if name == "" {
			continue
		}
		if resolved, ok := in.ExpressionAttributeNames[name]; ok {
			delete(item, resolved)
		} else {
			delete(item, name)
		}
	}
}

func staleLeaseRow(owner string, version int, ttlEpoch int64) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		attrPK:        &ddbtypes.AttributeValueMemberS{Value: leaseKey("l1")},
		attrOwner:     &ddbtypes.AttributeValueMemberS{Value: owner},
		attrVersion:   &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(version)},
		attrExpiresAt: &ddbtypes.AttributeValueMemberN{Value: "0"},
		// The hazard: a near-future ttl frozen by a pre-fix build.
		attrTTLName: &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(ttlEpoch, 10)},
	}
}

// MF-1 true-regression: a lease row carrying a stale legacy `ttl` (as an old
// build stamped) must have that attribute STRIPPED the first time the new
// code mutates it. Without `REMOVE #ttl` a TTL reaper would delete the
// actively-held row and reset its fencing version to 1.
func TestLeaseWrites_StripLegacyTTL(t *testing.T) {
	ctx := context.Background()
	const staleTTL int64 = 4102444800 // year 2100, epoch seconds

	t.Run("Renew_strips_stale_ttl", func(t *testing.T) {
		m := &mutatingClient{item: staleLeaseRow("owner-a", 1, staleTTL)}
		s := &Store{client: m, tableName: "leases-test", clk: clock.System}

		tok := persistence.LeaseToken{Version: 1, Owner: "owner-a"}
		if _, err := s.Renew(ctx, "l1", tok, time.Minute, nil); err != nil {
			t.Fatalf("renew: %v", err)
		}
		if _, ok := m.item[attrTTLName]; ok {
			t.Fatalf("Renew left a stale %q on the held row; a reaper could delete it and break fencing", attrTTLName)
		}
	})

	t.Run("Release_strips_stale_ttl", func(t *testing.T) {
		m := &mutatingClient{item: staleLeaseRow("owner-a", 1, staleTTL)}
		s := &Store{client: m, tableName: "leases-test", clk: clock.System}

		tok := persistence.LeaseToken{Version: 1, Owner: "owner-a"}
		if err := s.Release(ctx, "l1", tok); err != nil {
			t.Fatalf("release: %v", err)
		}
		if _, ok := m.item[attrTTLName]; ok {
			t.Fatalf("Release left a stale %q on the fencing row", attrTTLName)
		}
	})

	t.Run("Acquire_takeover_strips_stale_ttl", func(t *testing.T) {
		// Expired row (expires_at=0) owned by a former holder, carrying a
		// stale ttl. A new owner takes over via UpdateItem.
		m := &mutatingClient{item: staleLeaseRow("owner-old", 3, staleTTL)}
		s := &Store{client: m, tableName: "leases-test", clk: clock.System}

		if _, err := s.Acquire(ctx, "l1", "owner-new", time.Minute, nil); err != nil {
			t.Fatalf("acquire takeover: %v", err)
		}
		if _, ok := m.item[attrTTLName]; ok {
			t.Fatalf("Acquire takeover left a stale %q on the row", attrTTLName)
		}
	})
}

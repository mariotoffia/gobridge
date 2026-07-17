package dynamodblease

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// seizeClient is a dynamoAPI seam for the takeover-decision tests. It serves a
// single seeded lease row, models attribute_not_exists(#pk) on PutItem (a
// fresh acquire fails when the row already exists), and records/echoes
// UpdateItem takeovers (returning the incremented version so runTakeover can
// parse the resulting token). Condition evaluation is intentionally NOT
// modelled: these tests pin the store's control flow (fast path vs observation
// window), not DynamoDB's conditional-write semantics.
type seizeClient struct {
	item map[string]ddbtypes.AttributeValue

	updateCalls   int
	putCalls      int
	getCalls      int
	lastUpdateExp string
}

func (c *seizeClient) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	c.putCalls++
	if c.item != nil && in.ConditionExpression != nil &&
		strings.Contains(*in.ConditionExpression, "attribute_not_exists") {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	c.item = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (c *seizeClient) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if in.UpdateExpression != nil && strings.Contains(*in.UpdateExpression, "#obs_fp = :next_obs_fp") {
		applyObservationUpdate(c.item, in)
		return &dynamodb.UpdateItemOutput{Attributes: cloneObservationItem(c.item)}, nil
	}
	c.updateCalls++
	if in.ConditionExpression != nil {
		c.lastUpdateExp = *in.ConditionExpression
	}
	cur, _ := numAttr(c.item, attrVersion)
	next := cloneObservationItem(c.item)
	next[attrVersion] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatUint(cur+1, 10)}
	c.item = next
	return &dynamodb.UpdateItemOutput{Attributes: cloneObservationItem(next)}, nil
}

func (c *seizeClient) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	c.getCalls++
	return &dynamodb.GetItemOutput{Item: c.item}, nil
}

func (c *seizeClient) CreateTable(_ context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return &dynamodb.CreateTableOutput{}, nil
}

func (c *seizeClient) DescribeTable(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}

func (c *seizeClient) DescribeTimeToLive(_ context.Context, _ *dynamodb.DescribeTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error) {
	return &dynamodb.DescribeTimeToLiveOutput{}, nil
}

// heldRow builds an ACTIVELY-HELD lease row (non-empty owner, positive
// expires_at) so Acquire routes into the observation/fast-path takeover branch
// rather than the released-lease branch. renewedAt/expiresAt are epoch millis
// with expiresAt-renewedAt == ttl, so the observed TTL equals ttl.
func heldRow(owner string, version uint64, base time.Time, ttl time.Duration) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		attrPK:        &ddbtypes.AttributeValueMemberS{Value: leaseKey("l1")},
		attrOwner:     &ddbtypes.AttributeValueMemberS{Value: owner},
		attrVersion:   &ddbtypes.AttributeValueMemberN{Value: strconv.FormatUint(version, 10)},
		attrRenewedAt: &ddbtypes.AttributeValueMemberN{Value: millisStr(base)},
		attrExpiresAt: &ddbtypes.AttributeValueMemberN{Value: millisStr(base.Add(ttl))},
	}
}

// FIX 2: a crashed-and-restarted node (fresh Store instance, empty observation
// map) whose lease row still names it as owner must reclaim IMMEDIATELY —
// fenced on (owner, version) — without observing its own stale tuple for a full
// TTL. A node cannot race itself.
func TestAcquire_SameOwnerFastPath_SeizesWithoutObservationWindow(t *testing.T) {
	ctx := context.Background()
	ttl := 30 * time.Second
	base := time.Now().Add(-2 * ttl) // stale: last renewal is well in the past

	c := &seizeClient{item: heldRow("bridge-1", 5, base, ttl)}
	// Fresh store => empty observation map, as after a process restart.
	s := &Store{client: c, tableName: "leases-test", clk: clock.System}

	tok, err := s.Acquire(ctx, "l1", "bridge-1", ttl, nil)
	if err != nil {
		t.Fatalf("same-owner reacquire must seize immediately, got error: %v", err)
	}
	if c.updateCalls != 1 {
		t.Fatalf("same-owner reacquire must issue exactly one takeover UpdateItem (no observation wait), got %d", c.updateCalls)
	}
	if tok.Owner != "bridge-1" {
		t.Fatalf("token owner: got %q, want %q", tok.Owner, "bridge-1")
	}
	if tok.Version != 6 {
		t.Fatalf("token version must increment (5 -> 6), got %d", tok.Version)
	}
	// The seize is fenced on the exact (owner, version) just read.
	if !strings.Contains(c.lastUpdateExp, ":tuple_owner") || !strings.Contains(c.lastUpdateExp, ":tuple_version") {
		t.Fatalf("same-owner seize must fence on (owner, version), condition was %q", c.lastUpdateExp)
	}
}

// FIX 2 (negative): a DIFFERENT owner does NOT get the fast path — its first
// sighting starts the observation window and returns ErrAlreadyExists without
// issuing any takeover write.
func TestAcquire_DifferentOwner_FirstSightingObservesNoSeize(t *testing.T) {
	ctx := context.Background()
	ttl := 30 * time.Second
	base := time.Now().Add(-2 * ttl)

	c := &seizeClient{item: heldRow("bridge-1", 5, base, ttl)}
	s := &Store{client: c, tableName: "leases-test", clk: clock.System}

	_, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("different owner's first sighting must return ErrAlreadyExists (observing), got %v", err)
	}
	if c.updateCalls != 0 {
		t.Fatalf("different owner must NOT seize on first sighting; takeover UpdateItems = %d", c.updateCalls)
	}
}

// FIX 2 (window still enforced for a different owner): a different owner seizes
// only AFTER the observation window elapses on its own clock — proving the fast
// path did not weaken the cross-owner takeover guard.
func TestAcquire_DifferentOwner_SeizesAfterObservationWindow(t *testing.T) {
	ctx := context.Background()
	ttl := 30 * time.Second
	fake := clocktest.NewAt(time.Now())
	base := fake.Now().Add(-2 * ttl)

	c := &seizeClient{item: heldRow("bridge-1", 5, base, ttl)}
	s := &Store{client: c, tableName: "leases-test", clk: fake}

	// First sighting: observe, do not seize.
	if _, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first sighting must observe, got %v", err)
	}
	if c.updateCalls != 0 {
		t.Fatalf("no seize before the window elapses, got %d takeovers", c.updateCalls)
	}

	// Advance past the observed TTL (expires_at - renewed_at == ttl); the tuple
	// is unchanged, so the window is now satisfied.
	fake.Advance(ttl + time.Millisecond)

	tok, err := s.Acquire(ctx, "l1", "bridge-2", ttl, nil)
	if err != nil {
		t.Fatalf("different owner must seize after the window elapses, got %v", err)
	}
	if c.updateCalls != 1 {
		t.Fatalf("expected exactly one takeover after the window, got %d", c.updateCalls)
	}
	if tok.Owner != "bridge-2" || tok.Version != 6 {
		t.Fatalf("post-window token: got owner=%q ver=%d, want owner=bridge-2 ver=6", tok.Owner, tok.Version)
	}
}

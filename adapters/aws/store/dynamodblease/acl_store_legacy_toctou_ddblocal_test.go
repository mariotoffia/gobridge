package dynamodblease

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// reviveBeforeSeizeClient wraps a real DynamoDB client and fires a one-shot
// callback the instant BEFORE the takeover UpdateItem reaches DynamoDB. It
// injects — deterministically, with no timers — a concurrent write into the
// getRow -> UpdateItem gap that the seize's ConditionExpression must fence
// against. PutItem/GetItem/etc. delegate straight through (embedded dynamoAPI).
type reviveBeforeSeizeClient struct {
	dynamoAPI
	onUpdate func()
	fired    bool
}

func (c *reviveBeforeSeizeClient) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if !c.fired && c.onUpdate != nil {
		c.fired = true
		c.onUpdate()
	}
	return c.dynamoAPI.UpdateItem(ctx, in, opts...)
}

// TestAcquire_LegacyRowRevivesInSeizeGap_NotSeized is the TOCTOU split-brain
// regression (round-4). The observation-window reset closes STEADY STATE (a
// live legacy owner that keeps advancing expires_at never presents a frozen
// tuple), but it cannot close the gap between the store's final getRow and the
// seize UpdateItem: a foreign owner that was dead a full TTL and then REVIVES in
// that gap — advancing ONLY expires_at (owner+version unchanged, still no
// renewed_at, the most common minimal-lease writer shape) — leaves
// owner+version+attribute_not_exists(#ren) all satisfied, so WITHOUT an
// expires_at fence the standby would seize a now-LIVE owner → double ownership.
//
// The fix adds `#exp = :obs_expires` to the seize ConditionExpression. This test
// drives the race deterministically against REAL DynamoDB (only real conditional
// writes evaluate the fence) by advancing expires_at from inside the UpdateItem
// seam, exactly in the getRow->UpdateItem window.
//
// Mutation killed: drop the `#exp = :obs_expires` clause from the seize
// condition (acl_store.go observeOrSeize). The revived row still satisfies
// owner+version+attribute_not_exists(#ren), the UpdateItem SUCCEEDS, the standby
// seizes a LIVE owner, and this test FAILs at the "must NOT be seized" assertion
// (Acquire returns nil / owner becomes standby-1, version 8).
//
// ddblocal-gated: requires the DynamoDB Local container (ddblocal.Client).
func TestAcquire_LegacyRowRevivesInSeizeGap_NotSeized(t *testing.T) {
	real := ddblocal.Client(t)
	table := ddblocal.UniqueTable("leases-legacy-toctou")
	ctx := context.Background()

	ttl := 30 * time.Second
	fake := clocktest.NewAt(time.Now())

	// Create the table with a plain real-client store (setup never UpdateItems,
	// so the wrapper's one-shot reviver stays unfired until the real seize).
	if err := (&Store{client: real, tableName: table, clk: fake}).EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, real, table)

	const leaseID = "legacy-toctou"
	pk := leaseKey(leaseID)
	const owner = "reviving-legacy-owner"

	seed := func(exp time.Time) {
		if _, err := real.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: &table,
			Item: map[string]ddbtypes.AttributeValue{
				attrPK:        &ddbtypes.AttributeValueMemberS{Value: pk},
				attrOwner:     &ddbtypes.AttributeValueMemberS{Value: owner},
				attrVersion:   &ddbtypes.AttributeValueMemberN{Value: "7"},
				attrExpiresAt: &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(exp.UnixMilli(), 10)},
				// renewed_at intentionally ABSENT (legacy owner).
			},
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	frozen := fake.Now()
	seed(frozen)

	// The reviver: the dead owner comes back to life IN THE GAP, advancing ONLY
	// expires_at (owner + version unchanged, still no renewed_at). obs.expiresAt
	// was recorded as `frozen`; this makes the row's expires_at differ, so
	// `#exp = :obs_expires` must be FALSE and reject the seize.
	revived := frozen.Add(ttl)
	client := &reviveBeforeSeizeClient{
		dynamoAPI: real,
		onUpdate:  func() { seed(revived) },
	}
	s := &Store{client: client, tableName: table, clk: fake}

	// First sighting records the observation (owner, v7, renewed_at absent,
	// expires_at=frozen). No UpdateItem, so the reviver does NOT fire yet.
	if _, err := s.Acquire(ctx, leaseID, "standby-1", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first sighting must observe, got %v", err)
	}
	if client.fired {
		t.Fatal("reviver fired during first sighting (no UpdateItem should have run)")
	}

	// A full TTL of local time elapses on the FROZEN tuple: the standby now
	// believes the owner crashed and proceeds to seize.
	fake.Advance(ttl + time.Second)

	_, err := s.Acquire(ctx, leaseID, "standby-1", ttl, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("a foreign owner that REVIVED (advanced expires_at) in the getRow->UpdateItem "+
			"gap must NOT be seized; got err=%v — the seize must fence on #exp = :obs_expires", err)
	}
	if !client.fired {
		t.Fatal("test bug: the reviver never fired (the seize UpdateItem was not reached)")
	}

	// No ownership transfer: still the original owner at version 7, with the
	// reviver's advanced expires_at intact (a seize would set owner=standby-1,
	// version=8).
	got, err := real.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &table,
		Key:            map[string]ddbtypes.AttributeValue{attrPK: &ddbtypes.AttributeValueMemberS{Value: pk}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if own := strAttr(got.Item, attrOwner); own != owner {
		t.Fatalf("live reviver was seized (split-brain): owner=%q, want %q", own, owner)
	}
	if v, _ := numAttr(got.Item, attrVersion); v != 7 {
		t.Fatalf("fencing version changed (row was seized): got %d, want 7", v)
	}
}

// TestObserveOrSeize_LegacyRowNoExpiresAttr_AttributeNotExistsExpPermitsSeize is
// the obs.expiresAt==0 companion to the TOCTOU fix: a legacy row that carries NO
// expires_at attribute at all must still be seizable — the seize must fence with
// `attribute_not_exists(#exp)`, NOT `#exp = 0`. In DynamoDB `#exp = 0` compared
// against a MISSING attribute is FALSE and would wrongly BLOCK a legitimate
// seize; the symmetric absent-branch permits it.
//
// Acquire routes expires_at<=0 rows to the released fast path, so this defensive
// branch is unreachable end-to-end today; the test drives observeOrSeize
// directly (internal package) with a pre-recorded elapsed observation, exercising
// the branch against REAL DynamoDB conditional-write evaluation.
//
// Mutation killed: force the equality fence for obs.expiresAt==0 too (use
// `#exp = :obs_expires` unconditionally / drop the `if obs.expiresAt == 0`
// special-case). Against a row with no expires_at the equality is FALSE, the
// UpdateItem raises ConditionalCheckFailed → ErrAlreadyExists, and this test
// FAILs at the "must be seized" assertion.
//
// ddblocal-gated: requires the DynamoDB Local container (ddblocal.Client).
func TestObserveOrSeize_LegacyRowNoExpiresAttr_AttributeNotExistsExpPermitsSeize(t *testing.T) {
	real := ddblocal.Client(t)
	table := ddblocal.UniqueTable("leases-legacy-noexp")
	ctx := context.Background()

	ttl := 30 * time.Second
	fake := clocktest.NewAt(time.Now())
	s := &Store{client: real, tableName: table, clk: fake}
	if err := s.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, real, table)

	const leaseID = "legacy-noexp"
	pk := leaseKey(leaseID)
	const owner = "crashed-noexp-owner"

	// Seed a row with owner + version but NEITHER renewed_at NOR expires_at.
	if _, err := real.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &table,
		Item: map[string]ddbtypes.AttributeValue{
			attrPK:      &ddbtypes.AttributeValueMemberS{Value: pk},
			attrOwner:   &ddbtypes.AttributeValueMemberS{Value: owner},
			attrVersion: &ddbtypes.AttributeValueMemberN{Value: "7"},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Pre-record an ELAPSED observation of this exact (owner, v7, ren=0, exp=0)
	// tuple so observeOrSeize proceeds straight to the seize decision.
	s.recordObservation(leaseID, leaseObservation{
		owner:     owner,
		version:   7,
		renewedAt: 0,
		expiresAt: 0,
		firstSeen: fake.Now().Add(-2 * ttl),
	})
	row := leaseRow{present: true, owner: owner, version: 7, expiresAt: 0, renewedAt: 0}

	tok, err := s.observeOrSeize(ctx, leaseID, "standby-1", ttl, nil, fake.Now(), fake.Now().Add(ttl), row)
	if err != nil {
		t.Fatalf("a crashed legacy row with NO expires_at must be seized via "+
			"attribute_not_exists(#exp) (not `#exp = 0`, which is FALSE against a missing "+
			"attribute and would wrongly block a legitimate seize); got %v", err)
	}
	if tok.Owner != "standby-1" || tok.Version != 8 {
		t.Fatalf("takeover token: got owner=%q version=%d, want standby-1 version=8", tok.Owner, tok.Version)
	}

	// The seize healed the row: owner is now the standby at version 8.
	got, err := real.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &table,
		Key:            map[string]ddbtypes.AttributeValue{attrPK: &ddbtypes.AttributeValueMemberS{Value: pk}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if own := strAttr(got.Item, attrOwner); own != "standby-1" {
		t.Fatalf("seize did not transfer ownership: owner=%q, want standby-1", own)
	}
}

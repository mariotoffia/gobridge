package dynamodblease_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// TestAcquire_LegacyRowNoRenewedAt_StandbyTakesOverAfterWindow is the CRITICAL
// end-to-end regression (finding c13-legacy-renewedat), run against DynamoDB
// Local so REAL conditional-write evaluation happens — the seizeClient fake
// masked this bug by never evaluating the ConditionExpression.
//
// It seeds a genuine legacy row exactly as a pre-renewed_at build wrote it
// (owner set, positive expires_at, NO renewed_at attribute — a CRASHED legacy
// owner; a clean Release would empty owner and use the version-only fast path)
// and proves a standby ACTUALLY acquires it one TTL after first observing the
// (now final) tuple.
//
// Mutation killed: revert the seize condition to `#ren = :obs_ren` (drop the
// attribute_not_exists branch). Against real DynamoDB the equality fence is
// FALSE for the absent renewed_at, the UpdateItem raises
// ConditionalCheckFailedException → ErrAlreadyExists, the standby never acquires,
// and this test FAILs at the "must ACQUIRE" assertion (lease never taken over).
// The observedTTL mutation kills it too (the seize is never even reached).
//
// ddblocal-gated: requires the DynamoDB Local container (ddblocal.Client).
func TestAcquire_LegacyRowNoRenewedAt_StandbyTakesOverAfterWindow(t *testing.T) {
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable("leases-legacy-takeover")
	ctx := context.Background()

	ttl := 30 * time.Second
	// A fake clock lets the observation window elapse deterministically without
	// real sleeping; the store's takeover logic keys on s.clk only.
	fake := clocktest.NewAt(time.Now())
	store := dynamodblease.NewStore(client,
		dynamodblease.WithTableName(table),
		dynamodblease.WithClock(fake),
	)
	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)

	const leaseID = "legacy-orphan"
	const pk = "LEASE#" + leaseID
	// Seed the legacy row with NO renewed_at attribute.
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &table,
		Item: map[string]ddbtypes.AttributeValue{
			"PK":         &ddbtypes.AttributeValueMemberS{Value: pk},
			"owner":      &ddbtypes.AttributeValueMemberS{Value: "crashed-legacy-owner"},
			"version":    &ddbtypes.AttributeValueMemberN{Value: "7"},
			"expires_at": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(fake.Now().UnixMilli(), 10)},
			// renewed_at intentionally ABSENT.
		},
	}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// First sighting: the standby records its observation window; no takeover.
	if _, err := store.Acquire(ctx, leaseID, "standby-1", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first sighting of a legacy lease must observe, got %v", err)
	}

	// One TTL of local time elapses with the tuple unchanged (owner crashed).
	fake.Advance(ttl + time.Second)

	tok, err := store.Acquire(ctx, leaseID, "standby-1", ttl, nil)
	if err != nil {
		t.Fatalf("standby must ACQUIRE the orphaned legacy lease after the window, got %v "+
			"(a `#ren = :obs_ren` fence against an ABSENT renewed_at fails the conditional "+
			"write forever — the takeover must fence on attribute_not_exists(#ren))", err)
	}
	if tok.Owner != "standby-1" {
		t.Fatalf("token owner: got %q, want standby-1", tok.Owner)
	}
	if tok.Version != 8 {
		t.Fatalf("takeover must increment the fencing version 7 -> 8, got %d", tok.Version)
	}

	// The row is healed: renewed_at is now present and positive (one-shot legacy
	// upgrade), so a subsequent takeover uses the normal equality fence.
	got, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &table,
		Key:            map[string]ddbtypes.AttributeValue{"PK": &ddbtypes.AttributeValueMemberS{Value: pk}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("read healed row: %v", err)
	}
	ren, ok := got.Item["renewed_at"].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		t.Fatalf("takeover must WRITE renewed_at (heal the legacy row), item=%v", got.Item)
	}
	if ren.Value == "0" || ren.Value == "" {
		t.Fatalf("healed renewed_at must be positive, got %q", ren.Value)
	}
}

// TestAcquire_LegacyRowLiveRenewing_NotSeized is the [HIGH] split-brain-on-
// foreign-rows regression. The attribute_not_exists(#ren) takeover made a
// renewed_at-ABSENT row seizable, but a renewed_at-absent row exposes NO
// renewed_at liveness signal — so a FOREIGN owner that is still ALIVE and only
// advances expires_at (version unchanged, no renewed_at: a restore, a migration,
// a pre-repo binary, a hand-seed) would present a FROZEN (owner, version) tuple
// and get seized after one window → double ownership.
//
// The fix adds expires_at to the observation tuple, so a live legacy owner's
// advancing expiry RESETS the window and it is never seized. This is the LIVE
// side of the contrast; the CRASHED side (STATIC expires_at past one TTL IS
// seized-and-healed) is TestAcquire_LegacyRowNoRenewedAt_StandbyTakesOverAfterWindow
// above. Run against DynamoDB Local (real conditional writes / real row state).
//
// Mutation killed: omit expires_at from the observation tuple (revert the
// `obs.expiresAt != row.expiresAt` term in tupleChanged). The live owner's frozen
// (owner, version) tuple then satisfies the elapsed window, the standby SEIZES a
// LIVE owner, and this test FAILs at the "must NOT be seized" assertion.
//
// ddblocal-gated: requires the DynamoDB Local container (ddblocal.Client).
func TestAcquire_LegacyRowLiveRenewing_NotSeized(t *testing.T) {
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable("leases-legacy-live")
	ctx := context.Background()

	ttl := 30 * time.Second
	fake := clocktest.NewAt(time.Now())
	store := dynamodblease.NewStore(client,
		dynamodblease.WithTableName(table),
		dynamodblease.WithClock(fake),
	)
	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)

	const leaseID = "legacy-live"
	const pk = "LEASE#" + leaseID

	// seedLegacy writes a renewed_at-ABSENT row (owner + given expires_at, version
	// pinned at 7) — the state a live legacy owner leaves after each renewal.
	seedLegacy := func(exp time.Time) {
		if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: &table,
			Item: map[string]ddbtypes.AttributeValue{
				"PK":         &ddbtypes.AttributeValueMemberS{Value: pk},
				"owner":      &ddbtypes.AttributeValueMemberS{Value: "live-legacy-owner"},
				"version":    &ddbtypes.AttributeValueMemberN{Value: "7"},
				"expires_at": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(exp.UnixMilli(), 10)},
				// renewed_at intentionally ABSENT (legacy owner).
			},
		}); err != nil {
			t.Fatalf("seed legacy row: %v", err)
		}
	}

	// First sighting starts the observation window.
	seedLegacy(fake.Now().Add(ttl))
	if _, err := store.Acquire(ctx, leaseID, "standby-1", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first sighting must observe, got %v", err)
	}

	// The live owner keeps renewing (advancing expires_at) while MORE than a full
	// TTL of local time passes between polls. The standby must STILL only observe
	// on every poll, because expires_at moved each time (window reset).
	for i := 0; i < 3; i++ {
		fake.Advance(ttl + time.Second)
		seedLegacy(fake.Now().Add(ttl)) // live owner advances its expiry
		_, err := store.Acquire(ctx, leaseID, "standby-1", ttl, nil)
		if !errors.Is(err, shared.ErrAlreadyExists) {
			t.Fatalf("iteration %d: a LIVE legacy owner (expires_at advancing, version "+
				"unchanged, renewed_at absent) must NOT be seized — got %v; the observation "+
				"window must reset when expires_at moves", i, err)
		}
	}

	// The row must still belong to the original owner at version 7 (never seized).
	got, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &table,
		Key:            map[string]ddbtypes.AttributeValue{"PK": &ddbtypes.AttributeValueMemberS{Value: pk}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	own, _ := got.Item["owner"].(*ddbtypes.AttributeValueMemberS)
	if own == nil || own.Value != "live-legacy-owner" {
		t.Fatalf("live legacy owner was stolen (split-brain): owner=%v, want live-legacy-owner", got.Item["owner"])
	}
	ver, _ := got.Item["version"].(*ddbtypes.AttributeValueMemberN)
	if ver == nil || ver.Value != "7" {
		t.Fatalf("live legacy owner's fencing version changed (seized?): version=%v, want 7", got.Item["version"])
	}
}

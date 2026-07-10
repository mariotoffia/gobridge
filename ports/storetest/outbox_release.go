package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// RunOutboxReleaseTests exercises the OPTIONAL ports.OutboxReleaser
// capability against a store that implements it. Backends that support
// fast release (memory, SQLite, DynamoDB) run this suite in addition to
// RunOutboxStoreTests so release fencing behaves identically everywhere.
//
// The caller must pass a store that implements ports.OutboxReleaser; the
// suite fails fast otherwise.
func RunOutboxReleaseTests(t *testing.T, store ports.OutboxStore) {
	t.Helper()

	releaser, ok := store.(ports.OutboxReleaser)
	if !ok {
		t.Fatalf("store %T does not implement ports.OutboxReleaser", store)
	}

	t.Run("ReleaseReturnsRecordToPending", func(t *testing.T) {
		testReleaseReturnsRecordToPending(t, store, releaser)
	})
	t.Run("ReleaseWrongOwnerRejected", func(t *testing.T) {
		testReleaseWrongOwnerRejected(t, store, releaser)
	})
	t.Run("ReleaseUnclaimedRejected", func(t *testing.T) {
		testReleaseUnclaimedRejected(t, store, releaser)
	})
	t.Run("ReleaseRejectsZeroToken", func(t *testing.T) {
		testReleaseRejectsZeroToken(t, store, releaser)
	})
	t.Run("ReleaseRejectsZeroClaimSnapshot", func(t *testing.T) {
		testReleaseRejectsZeroClaimSnapshot(t, store, releaser)
	})
}

// testReleaseReturnsRecordToPending pins the fast-release round trip: a
// claimed record released with the claiming token is pending again and
// immediately re-claimable by the SAME token (no version bump, no
// stale-claim wait), with replay_count incremented by the second claim.
func testReleaseReturnsRecordToPending(t *testing.T, store ports.OutboxStore, releaser ports.OutboxReleaser) {
	ctx := context.Background()
	r := makeRecord(t, "rel-1", "env-rel-1", "bind-rel-1", "sess-rel", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	tok := persistence.LeaseToken{Version: 1, Owner: "owner-rel"}
	claimed, err := store.Claim(ctx, "SESSION#sess-rel", tok, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v, len=%d", err, len(claimed))
	}

	if err := releaser.Release(ctx, []string{"rel-1"}, tok); err != nil {
		t.Fatalf("release: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-rel", 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID() != "rel-1" {
		t.Fatalf("released record must be pending again, got %d records", len(pending))
	}
	if got := pending[0].ClaimedBy(); got != "" {
		t.Fatalf("released record ClaimedBy: got %q, want empty", got)
	}

	reclaimed, err := store.Claim(ctx, "SESSION#sess-rel", tok, 10)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("re-claim after release: err=%v, len=%d", err, len(reclaimed))
	}
	if got := reclaimed[0].ReplayCount(); got != 2 {
		t.Fatalf("replay count after release+reclaim: got %d, want 2", got)
	}
}

// testReleaseWrongOwnerRejected pins owner fencing: releasing with a token
// whose owner does not match the claim returns shared.ErrStaleFencingToken
// and leaves the record claimed.
func testReleaseWrongOwnerRejected(t *testing.T, store ports.OutboxStore, releaser ports.OutboxReleaser) {
	ctx := context.Background()
	r := makeRecord(t, "rel-wo-1", "env-rel-wo", "bind-rel-wo", "sess-rel-wo", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	tok := persistence.LeaseToken{Version: 1, Owner: "owner-a"}
	if _, err := store.Claim(ctx, "SESSION#sess-rel-wo", tok, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	wrong := persistence.LeaseToken{Version: 1, Owner: "owner-b"}
	err := releaser.Release(ctx, []string{"rel-wo-1"}, wrong)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken for wrong owner, got %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-rel-wo", 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("record must stay claimed after rejected release, got %d pending", len(pending))
	}
}

// testReleaseUnclaimedRejected pins that Release is claim-scoped, not
// idempotent: releasing a record that is pending (never claimed, or
// already released) is a status mismatch and returns
// shared.ErrStaleFencingToken.
func testReleaseUnclaimedRejected(t *testing.T, store ports.OutboxStore, releaser ports.OutboxReleaser) {
	ctx := context.Background()
	r := makeRecord(t, "rel-un-1", "env-rel-un", "bind-rel-un", "sess-rel-un", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	tok := persistence.LeaseToken{Version: 1, Owner: "owner-un"}
	err := releaser.Release(ctx, []string{"rel-un-1"}, tok)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken releasing an unclaimed record, got %v", err)
	}
}

// testReleaseRejectsZeroToken pins the valid-token requirement on Release:
// Release fencing is owner+version+status, so a zero-value / invalid
// LeaseToken (empty owner or zero version) can never match a real claim and is
// rejected with shared.ErrStaleFencingToken, leaving the record claimed for its
// rightful owner.
func testReleaseRejectsZeroToken(t *testing.T, store ports.OutboxStore, releaser ports.OutboxReleaser) {
	ctx := context.Background()
	r := makeRecord(t, "rel-zt-1", "env-rel-zt", "bind-rel-zt", "sess-rel-zt", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	tok := persistence.LeaseToken{Version: 1, Owner: "owner-rel-zt"}
	if _, err := store.Claim(ctx, "SESSION#sess-rel-zt", tok, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	for _, invalid := range []persistence.LeaseToken{
		{},
		{Version: 1},
		{Owner: "owner-rel-zt"},
	} {
		if err := releaser.Release(ctx, []string{"rel-zt-1"}, invalid); !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("expected ErrStaleFencingToken releasing with invalid token %+v, got %v", invalid, err)
		}
	}

	// Still claimed by its rightful owner: the valid token releases it.
	if err := releaser.Release(ctx, []string{"rel-zt-1"}, tok); err != nil {
		t.Fatalf("rightful release after rejected zero tokens: %v", err)
	}
}

// testReleaseRejectsZeroClaimSnapshot is the Release-side twin of
// testCompleteRejectsZeroClaimSnapshot: a zero-claimed row (Status == Claimed,
// claimed_by == "", claim_version == 0) presented with a zero-value
// LeaseToken{} must be rejected with shared.ErrStaleFencingToken and must NOT
// be returned to pending. Stores routing Release through the aggregate inherit
// the self-guard; raw-SQL/DDB stores must add an explicit invalid-token guard.
// Backends that cannot represent a zero-claimed row through the port skip.
func testReleaseRejectsZeroClaimSnapshot(t *testing.T, store ports.OutboxStore, releaser ports.OutboxReleaser) {
	ctx := context.Background()
	pk, ok := seedZeroClaimed(t, store, "zrs-1", "sess-zrs")
	if !ok {
		t.Skip("backend force-resets status to pending on Persist; a zero-claimed row is not representable through the port (aggregate-routed stores are covered by the OutboxRecord self-guard; raw-SQL/DDB stores must add an explicit invalid-token guard on Release)")
	}

	if err := releaser.Release(ctx, []string{"zrs-1"}, persistence.LeaseToken{}); !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken releasing a zero-claimed row with a zero token, got %v", err)
	}

	// Non-mutation: Release's only possible mutation is Claimed -> Pending, so
	// the row must NOT have surfaced as pending.
	pending, err := store.QueryPending(ctx, pk, 10)
	if err != nil {
		t.Fatalf("query pending after rejected release: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("zero-claimed row must not become pending after rejected release, got %d", len(pending))
	}
}

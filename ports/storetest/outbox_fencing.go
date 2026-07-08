package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Owner-divergence and fencing conformance cases. These pin the unified
// outbox fencing contract: claim authority comes solely from token.Owner,
// completion is owner+version+status fenced, and expiry is pending-only.

// testClaimSetsOwnerFromToken proves the claim owner is sourced solely from
// token.Owner. There is no separate ownerID authority, so claimed_by must
// equal token.Owner after a successful claim.
func testClaimSetsOwnerFromToken(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "owndiv-1", "env-owndiv-1", "bind-owndiv-1", "sess-owndiv", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-from-token"}
	claimed, err := store.Claim(ctx, "SESSION#sess-owndiv", token, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}
	if claimed[0].ClaimedBy() != token.Owner {
		t.Fatalf("claimedBy: got %q, want %q (token.Owner)", claimed[0].ClaimedBy(), token.Owner)
	}
}

// testCompleteRejectsSameVersionDifferentOwner proves the completion fence is
// owner+version+status, not version-only: a complete carrying the right version
// but a different owner must be rejected with ErrStaleFencingToken and must
// leave the record claimed (the rightful owner can still complete it).
func testCompleteRejectsSameVersionDifferentOwner(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "ownc-1", "env-ownc-1", "bind-ownc-1", "sess-ownc", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	owner := persistence.LeaseToken{Version: 7, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-ownc", owner, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	imposter := persistence.LeaseToken{Version: 7, Owner: "owner-B"}
	if err := store.Complete(ctx, []string{"ownc-1"}, imposter); !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken for same-version/different-owner complete, got %v", err)
	}

	// The record was left claimed, so the rightful owner can still complete it.
	if err := store.Complete(ctx, []string{"ownc-1"}, owner); err != nil {
		t.Fatalf("rightful complete after rejected imposter: %v", err)
	}
}

// testExpireSkipsClaimed proves Expire is pending-only: a claimed record past
// its expiry must not be expired nor counted, and must remain claimed.
func testExpireSkipsClaimed(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	past := time.Now().Add(-1 * time.Hour)
	r := makeRecord(t, "expcl-1", "env-expcl-1", "bind-expcl-1", "sess-expcl", "route-1", past)
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-expcl", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	n, err := store.Expire(ctx, time.Now(), "SESSION#sess-expcl")
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 expired (record is claimed, not pending), got %d", n)
	}

	// The record was left claimed, so its owner can still complete it.
	if err := store.Complete(ctx, []string{"expcl-1"}, token); err != nil {
		t.Fatalf("complete claimed record after expire skip: %v", err)
	}
}

// testClaimRejectsStaleVersionOnPending proves the claim fence is monotonic on
// the partition: once a partition has observed a newer fencing-token version (a
// claim + complete at v2), a later claim attempt carrying an OLDER token.Version
// must NOT succeed in claiming a freshly persisted pending row. The stale holder
// is preempted, so it gets no records back.
func testClaimRejectsStaleVersionOnPending(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	pk := "SESSION#sess-stalever"

	// Advance the partition's observed fencing version to 2 by claiming and
	// completing a first record at v2.
	r1 := makeRecord(t, "stalever-1", "env-stalever-1", "bind-stalever-1", "sess-stalever", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r1}); err != nil {
		t.Fatalf("persist r1: %v", err)
	}
	v2 := persistence.LeaseToken{Version: 2, Owner: "owner-v2"}
	claimed, err := store.Claim(ctx, pk, v2, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim at v2: err=%v, len=%d", err, len(claimed))
	}
	if err := store.Complete(ctx, []string{"stalever-1"}, v2); err != nil {
		t.Fatalf("complete at v2: %v", err)
	}

	// A fresh pending row arrives. A claim with an OLDER token (v1) must not
	// win it: the partition has already moved past v1.
	r2 := makeRecord(t, "stalever-2", "env-stalever-2", "bind-stalever-2", "sess-stalever", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r2}); err != nil {
		t.Fatalf("persist r2: %v", err)
	}
	v1 := persistence.LeaseToken{Version: 1, Owner: "owner-v1"}
	stale, err := store.Claim(ctx, pk, v1, 10)
	if err != nil && !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("stale claim: unexpected error %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("expected 0 records claimed by stale (v1) token, got %d", len(stale))
	}
}

// testClaimRejectsStaleVersionAfterNoopHigherClaim proves the per-partition
// fencing high-water-mark is durable and advances on EVERY claim, including a
// no-op claim that returns zero records. A claim at v5 against an empty
// partition observes no rows yet must still raise the partition's fence to 5;
// a later stale (v1) claim against a freshly-arrived pending row must then be
// rejected (get zero records). Without a durable high-water-mark, a backend
// that derives the fence from persisted rows alone forgets the no-op v5 and
// lets the stale v1 token win the new work.
func testClaimRejectsStaleVersionAfterNoopHigherClaim(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	pk := "SESSION#sess-noopfence"

	// (a) Claim an EMPTY partition at v5. No records exist yet, so this is a
	// no-op claim: zero records, no error. It must still advance the fence.
	v5 := persistence.LeaseToken{Version: 5, Owner: "owner-v5"}
	claimed, err := store.Claim(ctx, pk, v5, 10)
	if err != nil {
		t.Fatalf("no-op claim at v5: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("expected 0 records from no-op claim on empty partition, got %d", len(claimed))
	}

	// (b) A fresh pending record now arrives in the same partition.
	r := makeRecord(t, "noopfence-1", "env-noopfence-1", "bind-noopfence-1", "sess-noopfence", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// (c) A claim with a STALE token (v1) must not win the new row: the
	// partition's fence already moved to 5 on the no-op claim.
	v1 := persistence.LeaseToken{Version: 1, Owner: "owner-v1"}
	stale, err := store.Claim(ctx, pk, v1, 10)
	if err != nil && !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("stale claim: unexpected error %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("expected 0 records claimed by stale (v1) token after no-op v5 claim, got %d", len(stale))
	}
}

// testClaimReturnsSameKeyRecordsInCreatedOrder proves Claim returns records
// sharing one ordering key (messaging.HeaderOrderingKey) in created_at order, so
// the drainer can deliver per-key in-order. Records are persisted out of order
// to ensure the store sorts rather than echoing insertion order.
func testClaimReturnsSameKeyRecordsInCreatedOrder(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	const orderingKey = "device-order-1"
	base := time.Now().Add(-time.Hour).UTC()

	// created_at offsets in seconds, persisted out of order; expected claim
	// order is ascending created_at, i.e. ids by ascending offset.
	type spec struct {
		id     string
		offset int
	}
	specs := []spec{
		{"sko-c", 30},
		{"sko-a", 10},
		{"sko-d", 40},
		{"sko-b", 20},
	}
	for _, s := range specs {
		createdAt := base.Add(time.Duration(s.offset) * time.Second)
		rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
			ID:         s.id,
			EnvelopeID: "env-" + s.id,
			BindingID:  "bind-" + s.id,
			SessionID:  "sess-sko",
			Address:    "test/topic",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:          "env-" + s.id,
				Subject:     "test-subject",
				Payload:     []byte(`{"data":"value"}`),
				OrderingKey: orderingKey,
			}),
			CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("new record %s: %v", s.id, err)
		}
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", s.id, err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-sko"}
	claimed, err := store.Claim(ctx, "SESSION#sess-sko", token, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != len(specs) {
		t.Fatalf("expected %d claimed, got %d", len(specs), len(claimed))
	}

	want := []string{"sko-a", "sko-b", "sko-c", "sko-d"}
	for i, rec := range claimed {
		if got, ok := rec.OrderingKey(); !ok || got != orderingKey {
			t.Fatalf("record %d: ordering key got %q (present=%v), want %q", i, got, ok, orderingKey)
		}
		if rec.ID() != want[i] {
			t.Fatalf("claim order at %d: got %q, want %q (records must be in created_at order)", i, rec.ID(), want[i])
		}
	}
}

// testClaimTieBreaksByPersistOrderOnEqualCreatedAt verifies that records
// sharing an ordering key AND an identical created_at are claimed in persist
// order: the store assigns a monotonic per-partition sequence at Persist
// (persistence.OutboxSnapshot.Seq) and orders claims by ascending
// (created_at, seq). Two same-key messages published within the same
// millisecond must therefore drain in the order they were persisted on every
// backend — never in envelope-ID (random hex) order.
func testClaimTieBreaksByPersistOrderOnEqualCreatedAt(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	const orderingKey = "device-tie-1"
	createdAt := time.Now().Add(-time.Hour).UTC()

	// All records share one created_at; envelope IDs are deliberately in
	// DESCENDING lexical order relative to persist order, so a backend that
	// still tie-breaks by envelope ID fails. Expected claim order is persist
	// order: tie-c, tie-a, tie-b.
	ids := []string{"tie-c", "tie-a", "tie-b"}
	for _, id := range ids {
		rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
			ID:         id,
			EnvelopeID: "env-" + id,
			BindingID:  "bind-" + id,
			SessionID:  "sess-tie",
			Address:    "test/topic",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:          "env-" + id,
				Subject:     "test-subject",
				Payload:     []byte(`{"data":"value"}`),
				OrderingKey: orderingKey,
			}),
			CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("new record %s: %v", id, err)
		}
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", id, err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-tie"}
	claimed, err := store.Claim(ctx, "SESSION#sess-tie", token, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != len(ids) {
		t.Fatalf("expected %d claimed, got %d", len(ids), len(claimed))
	}

	for i, rec := range claimed {
		if rec.ID() != ids[i] {
			t.Fatalf("tie-break order at %d: got %q, want %q (equal created_at must order by persist sequence)", i, rec.ID(), ids[i])
		}
	}
	// The store-assigned sequence must be strictly increasing in persist
	// order — it is the durable tiebreaker, not an in-memory artifact.
	for i := 1; i < len(claimed); i++ {
		if claimed[i].Seq() <= claimed[i-1].Seq() {
			t.Fatalf("seq not strictly increasing in persist order: seq[%d]=%d, seq[%d]=%d",
				i-1, claimed[i-1].Seq(), i, claimed[i].Seq())
		}
	}
}

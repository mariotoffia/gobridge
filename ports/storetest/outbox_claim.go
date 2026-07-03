package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Claim-semantics conformance cases: non-positive limits, fence
// interleaving between competing owners, and concurrent-claimer
// exactly-once guarantees.

// testClaimZeroLimitAdvancesFenceOnly pins the limit <= 0 contract: the call
// is a fencing no-op — it validates the token and durably advances the
// per-partition high-water-mark exactly like a claim against an empty
// partition, but claims and returns no records. Backends historically
// diverged here (none / all / driver-defined), which broke drainer batch
// accounting.
func testClaimZeroLimitAdvancesFenceOnly(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	pk := "SESSION#sess-zlim"

	r := makeRecord(t, "zlim-1", "env-zlim-1", "bind-zlim-1", "sess-zlim", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	v5 := persistence.LeaseToken{Version: 5, Owner: "owner-v5"}
	for _, limit := range []int{0, -1} {
		claimed, err := store.Claim(ctx, pk, v5, limit)
		if err != nil {
			t.Fatalf("claim limit=%d: %v", limit, err)
		}
		if len(claimed) != 0 {
			t.Fatalf("claim limit=%d: expected 0 records, got %d", limit, len(claimed))
		}
	}

	// The record was not claimed.
	pending, err := store.QueryPending(ctx, pk, 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 || pending[0].Status() != persistence.OutboxPending {
		t.Fatalf("expected 1 still-pending record after zero-limit claims, got %d", len(pending))
	}

	// The fence WAS advanced: a stale (v1) claim must not win the record.
	v1 := persistence.LeaseToken{Version: 1, Owner: "owner-v1"}
	stale, err := store.Claim(ctx, pk, v1, 10)
	if err != nil && !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("stale claim: unexpected error %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("expected 0 records for stale token after zero-limit fence advance, got %d", len(stale))
	}

	// The fencing owner itself can still claim at its own version.
	claimed, err := store.Claim(ctx, pk, v5, 10)
	if err != nil {
		t.Fatalf("claim at fence version: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 record claimed at fence version, got %d", len(claimed))
	}
}

// testClaimFenceInterleaving replays the split-brain sequence from the
// DynamoDB fence TOCTOU finding, serialized: owner A (v5) claims part of the
// partition, owner B (v6) raises the fence by claiming, then A tries to claim
// the REMAINING pending records at v5. A must get zero records (or an
// explicit ErrStaleFencingToken) — every claim must be fenced against the
// high-water-mark at claim time, not against a fence read at the start of a
// batch.
func testClaimFenceInterleaving(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	pk := "SESSION#sess-ilv"

	base := time.Now().Add(-time.Hour).UTC()
	for i := 0; i < 3; i++ {
		rec := makeRecord(t, "ilv-"+itoa(i), "env-ilv-"+itoa(i), "bind-ilv-"+itoa(i), "sess-ilv", "route-1", time.Time{})
		snap := rec.PersistenceSnapshot()
		snap.CreatedAt = base.Add(time.Duration(i) * time.Second)
		rec = persistence.RehydrateFromSnapshot(snap)
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist ilv-%d: %v", i, err)
		}
	}

	vA := persistence.LeaseToken{Version: 5, Owner: "owner-A"}
	vB := persistence.LeaseToken{Version: 6, Owner: "owner-B"}

	// A claims one record at v5 — legitimate.
	got, err := store.Claim(ctx, pk, vA, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("A first claim: err=%v, len=%d", err, len(got))
	}

	// B preempts: claims one record at v6, raising the partition fence.
	got, err = store.Claim(ctx, pk, vB, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("B claim: err=%v, len=%d", err, len(got))
	}

	// A tries to keep claiming the remaining pending record at v5. The fence
	// has moved to 6; A must win nothing.
	stale, err := store.Claim(ctx, pk, vA, 10)
	if err != nil && !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("A post-preemption claim: unexpected error %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("split-brain: preempted owner A claimed %d records after B raised the fence", len(stale))
	}

	// B can still drain the remainder: the untouched pending record AND the
	// record A claimed at v5 (its claim version is strictly older than 6, so
	// B reclaims the preempted work).
	rest, err := store.Claim(ctx, pk, vB, 10)
	if err != nil {
		t.Fatalf("B drain remainder: %v", err)
	}
	if len(rest) != 2 {
		t.Fatalf("expected B to claim the remaining pending record plus A's preempted record (2), got %d", len(rest))
	}
}

// testConcurrentClaimersClaimEachRecordOnce proves claim atomicity under
// concurrency: two claimers racing with the SAME token must partition the
// pending set — every record is claimed exactly once, none twice, none lost.
// A backend whose pending→claimed transition is not conditional hands the
// same record to both claimers and the drainer double-delivers.
func testConcurrentClaimersClaimEachRecordOnce(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	pk := "SESSION#sess-cc"
	const total = 20

	for i := 0; i < total; i++ {
		r := makeRecord(t, "cc-"+itoa(i), "env-cc-"+itoa(i), "bind-cc-"+itoa(i), "sess-cc", "route-1", time.Time{})
		if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
			t.Fatalf("persist cc-%d: %v", i, err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-cc"}
	results := make([][]*persistence.OutboxRecord, 2)
	errs := make([]error, 2)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			results[slot], errs[slot] = store.Claim(ctx, pk, token, total)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("claimer %d: unexpected error %v", i, err)
		}
	}

	seen := make(map[string]int, total)
	for _, recs := range results {
		for _, r := range recs {
			seen[r.ID()]++
		}
	}
	if len(seen) != total {
		t.Fatalf("expected all %d records claimed across both claimers, got %d", total, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("record %s claimed %d times; want exactly once", id, n)
		}
	}
}

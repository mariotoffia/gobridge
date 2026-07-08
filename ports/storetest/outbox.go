// Package storetest provides conformance test suites for ports.OutboxStore
// implementations. Both memoryoutbox and sqliteoutbox use these to ensure
// identical behavior across backends.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func makeRecord(t *testing.T, id, envelopeID, bindingID, sessionID, routeID string, expiresAt time.Time) *persistence.OutboxRecord {
	t.Helper()
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    routeID,
		EnvelopeID: envelopeID,
		BindingID:  bindingID,
		SessionID:  sessionID,
		Address:    "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      envelopeID,
			Subject: "test-subject",
			Payload: []byte(`{"data":"value"}`),
			Headers: map[string]any{"key": "val"},
		}),
		DispatchHeaders: map[string]any{"dispatch": "header"},
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		t.Fatalf("makeRecord: %v", err)
	}
	return rec
}

// RunOutboxStoreTests executes the full conformance suite against the given
// OutboxStore. The caller is responsible for creating and closing the store.
func RunOutboxStoreTests(t *testing.T, store ports.OutboxStore) {
	t.Helper()

	t.Run("PersistAndQueryPending", func(t *testing.T) { testPersistAndQueryPending(t, store) })
	t.Run("PersistDuplicate", func(t *testing.T) { testPersistDuplicate(t, store) })
	t.Run("PersistIdentityPartitionScoped", func(t *testing.T) { testPersistIdentityPartitionScoped(t, store) })
	t.Run("PersistFanOut", func(t *testing.T) { testPersistFanOut(t, store) })
	t.Run("PersistPartialOverlap", func(t *testing.T) { testPersistPartialOverlap(t, store) })
	t.Run("PersistBatchInternalDuplicate", func(t *testing.T) { testPersistBatchInternalDuplicate(t, store) })
	t.Run("ClaimTransitionsStatus", func(t *testing.T) { testClaimTransitionsStatus(t, store) })
	t.Run("ClaimRespectsLimit", func(t *testing.T) { testClaimRespectsLimit(t, store) })
	t.Run("ClaimLimitReturnsOldestN", func(t *testing.T) { testClaimLimitReturnsOldestN(t, store) })
	t.Run("ClaimVersionPreemptsPriorOwner", func(t *testing.T) { testClaimVersionPreemptsPriorOwner(t, store) })
	t.Run("CompleteWithValidToken", func(t *testing.T) { testCompleteWithValidToken(t, store) })
	t.Run("CompleteWithWrongToken", func(t *testing.T) { testCompleteWithWrongToken(t, store) })
	t.Run("ExpireMarksEligible", func(t *testing.T) { testExpireMarksEligible(t, store) })
	t.Run("ExpireSkipsCompleted", func(t *testing.T) { testExpireSkipsCompleted(t, store) })
	t.Run("ExpireScopedToPartition", func(t *testing.T) { testExpireScopedToPartition(t, store) })
	t.Run("QueryPendingOnlyPending", func(t *testing.T) { testQueryPendingOnlyPending(t, store) })
	t.Run("QueryPendingRespectsPartitionKey", func(t *testing.T) { testQueryPendingRespectsPartitionKey(t, store) })
	t.Run("FullLifecycle", func(t *testing.T) { testFullLifecycle(t, store) })
	t.Run("IdempotentPersist", func(t *testing.T) { testIdempotentPersist(t, store) })
	t.Run("CompleteAfterTokenChange", func(t *testing.T) { outboxCompleteAfterTokenChange(t, store) })
	t.Run("ClaimSetsOwnerFromToken", func(t *testing.T) { testClaimSetsOwnerFromToken(t, store) })
	t.Run("CompleteRejectsSameVersionDifferentOwner", func(t *testing.T) { testCompleteRejectsSameVersionDifferentOwner(t, store) })
	t.Run("ExpireSkipsClaimed", func(t *testing.T) { testExpireSkipsClaimed(t, store) })
	t.Run("ClaimRejectsStaleVersionOnPending", func(t *testing.T) { testClaimRejectsStaleVersionOnPending(t, store) })
	t.Run("ClaimRejectsStaleVersionAfterNoopHigherClaim", func(t *testing.T) { testClaimRejectsStaleVersionAfterNoopHigherClaim(t, store) })
	t.Run("ClaimReturnsSameKeyRecordsInCreatedOrder", func(t *testing.T) { testClaimReturnsSameKeyRecordsInCreatedOrder(t, store) })
	t.Run("ClaimTieBreaksByPersistOrderOnEqualCreatedAt", func(t *testing.T) { testClaimTieBreaksByPersistOrderOnEqualCreatedAt(t, store) })
	t.Run("ClaimZeroLimitAdvancesFenceOnly", func(t *testing.T) { testClaimZeroLimitAdvancesFenceOnly(t, store) })
	t.Run("ClaimBeyondReplayCapRemainsClaimable", func(t *testing.T) { testClaimBeyondReplayCapRemainsClaimable(t, store) })
	t.Run("ClaimFenceInterleaving", func(t *testing.T) { testClaimFenceInterleaving(t, store) })
	t.Run("ConcurrentClaimersClaimEachRecordOnce", func(t *testing.T) { testConcurrentClaimersClaimEachRecordOnce(t, store) })
}

func testPersistAndQueryPending(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "pq-1", "env-pq-1", "bind-pq-1", "sess-pq", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	results, err := store.QueryPending(ctx, "SESSION#sess-pq", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 record, got %d", len(results))
	}
	if results[0].ID() != "pq-1" {
		t.Fatalf("id: got %q, want %q", results[0].ID(), "pq-1")
	}
	if results[0].Status() != persistence.OutboxPending {
		t.Fatalf("status: got %q, want %q", results[0].Status(), persistence.OutboxPending)
	}
	env := results[0].Snapshot()
	if env.ID() != "env-pq-1" {
		t.Fatalf("envelope.ID: got %q, want %q", env.ID(), "env-pq-1")
	}
	if string(env.Payload()) != `{"data":"value"}` {
		t.Fatalf("payload mismatch: %q", env.Payload())
	}
}

func testPersistDuplicate(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "dup-1", "env-dup", "bind-dup", "sess-dup", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	r2 := makeRecord(t, "dup-2", "env-dup", "bind-dup", "sess-dup", "route-1", time.Time{})
	err := store.Persist(ctx, []*persistence.OutboxRecord{r2})
	if !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord, got %v", err)
	}
}

// testPersistPartialOverlap pins the per-record Persist idempotency contract
// (C1): a batch that partially overlaps already-persisted records persists
// the new records and silently skips the existing ones; ErrDuplicateRecord
// is returned ONLY when every record in the batch already existed. This is
// what makes a fan-out re-persist after a partial failure safe: without it,
// the retried batch fails all-or-nothing and the unpersisted legs are ACKed
// away — a verified message-loss sequence.
func testPersistPartialOverlap(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()

	a := makeRecord(t, "po-a", "env-po-a", "bind-po-a", "sess-po", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{a}); err != nil {
		t.Fatalf("persist {A}: %v", err)
	}

	// Re-persist A (same envelope+binding identity, new record instance)
	// together with a brand-new B: B must be persisted, no error returned.
	a2 := makeRecord(t, "po-a-retry", "env-po-a", "bind-po-a", "sess-po", "route-1", time.Time{})
	b := makeRecord(t, "po-b", "env-po-b", "bind-po-b", "sess-po", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{a2, b}); err != nil {
		t.Fatalf("persist {A,B} with A existing: want nil error, got %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-po", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending (A kept once, B persisted), got %d", len(pending))
	}
	ids := map[string]bool{}
	for _, r := range pending {
		ids[r.ID()] = true
	}
	if !ids["po-a"] {
		t.Fatal("original record po-a must be retained (skip, not overwrite)")
	}
	if !ids["po-b"] {
		t.Fatal("new record po-b must be persisted despite duplicate sibling")
	}

	// Re-persisting ONLY the existing identity is an all-duplicate batch.
	a3 := makeRecord(t, "po-a-again", "env-po-a", "bind-po-a", "sess-po", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{a3}); !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord for all-duplicate batch, got %v", err)
	}
}

// testPersistBatchInternalDuplicate pins the per-record rule for a batch that
// contains the same identity twice: the first occurrence is persisted, the
// rest are skipped, and no error is returned (the batch persisted new work).
func testPersistBatchInternalDuplicate(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()

	r1 := makeRecord(t, "bid-1", "env-bid", "bind-bid", "sess-bid", "route-1", time.Time{})
	r2 := makeRecord(t, "bid-2", "env-bid", "bind-bid", "sess-bid", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r1, r2}); err != nil {
		t.Fatalf("persist batch with internal duplicate: want nil error, got %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-bid", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending (first occurrence wins), got %d", len(pending))
	}
	if pending[0].ID() != "bid-1" {
		t.Fatalf("expected first occurrence bid-1 persisted, got %q", pending[0].ID())
	}
}

func testPersistFanOut(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	records := []*persistence.OutboxRecord{
		makeRecord(t, "fo-1", "env-fo", "bind-A", "sess-fo", "route-1", time.Time{}),
		makeRecord(t, "fo-2", "env-fo", "bind-B", "sess-fo", "route-1", time.Time{}),
		makeRecord(t, "fo-3", "env-fo", "bind-C", "sess-fo", "route-1", time.Time{}),
	}
	if err := store.Persist(ctx, records); err != nil {
		t.Fatalf("fan-out persist: %v", err)
	}

	results, err := store.QueryPending(ctx, "SESSION#sess-fo", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 records, got %d", len(results))
	}
}

func testClaimTransitionsStatus(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "cl-1", "env-cl-1", "bind-cl-1", "sess-cl", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, "SESSION#sess-cl", token, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}
	if claimed[0].Status() != persistence.OutboxClaimed {
		t.Fatalf("status: got %q, want %q", claimed[0].Status(), persistence.OutboxClaimed)
	}
	if claimed[0].ClaimedBy() != "owner-A" {
		t.Fatalf("claimedBy: got %q, want %q", claimed[0].ClaimedBy(), "owner-A")
	}
	if claimed[0].ClaimVersion() != 1 {
		t.Fatalf("claimVersion: got %d, want %d", claimed[0].ClaimVersion(), 1)
	}
	if claimed[0].ReplayCount() != 1 {
		t.Fatalf("replayCount: got %d, want %d", claimed[0].ReplayCount(), 1)
	}
}

func testClaimRespectsLimit(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		r := makeRecord(t,
			"clim-"+itoa(i), "env-clim-"+itoa(i), "bind-clim-"+itoa(i),
			"sess-clim", "route-1", time.Time{},
		)
		if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, "SESSION#sess-clim", token, 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("expected 3, got %d", len(claimed))
	}
}

// testClaimLimitReturnsOldestN pins WHICH records a limited Claim selects, not
// merely how many (that is testClaimRespectsLimit). When more records are
// claimable than `limit`, Claim must return exactly the OLDEST-N by ascending
// (created_at, seq) — the same set `ORDER BY created_at, seq LIMIT ?` yields on
// the SQL backends — and in ascending order. The envelope IDs here are
// deliberately assigned in DESCENDING lexical order relative to age, so a
// backend that selects the first-N in key/envelope-ID order (as a naive
// DynamoDB Query+Limit does) returns the NEWEST records and fails: under a
// sustained backlog that starves the oldest records indefinitely and reorders
// ordering-sensitive partitions. A same-created_at pair (olderN-1/olderN-2)
// forces the seq tiebreak to decide the boundary, not the timestamp alone.
func testClaimLimitReturnsOldestN(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).UTC()

	// Persist order fixes seq (ascending). offset is the created_at delta in
	// seconds; env is the envelope ID, assigned so lexical order is the INVERSE
	// of age (env-oldn-8 is oldest, env-oldn-4 is newest).
	type spec struct {
		id     string
		offset int
		env    string
	}
	// olderN-1 and olderN-2 share created_at (offset 10); seq (persist order)
	// breaks the tie so olderN-1 (persisted first) sorts before olderN-2.
	specs := []spec{
		{"oldn-1", 10, "env-oldn-8"},
		{"oldn-2", 10, "env-oldn-7"},
		{"oldn-3", 20, "env-oldn-6"},
		{"oldn-4", 30, "env-oldn-5"},
		{"oldn-5", 40, "env-oldn-4"},
	}
	for _, s := range specs {
		rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
			ID:         s.id,
			EnvelopeID: s.env,
			BindingID:  "bind-" + s.id,
			SessionID:  "sess-oldn",
			Address:    "test/topic",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      s.env,
				Subject: "test-subject",
				Payload: []byte(`{"data":"value"}`),
			}),
			CreatedAt: base.Add(time.Duration(s.offset) * time.Second),
		})
		if err != nil {
			t.Fatalf("new record %s: %v", s.id, err)
		}
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", s.id, err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-oldn"}
	claimed, err := store.Claim(ctx, "SESSION#sess-oldn", token, 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("expected 3 claimed, got %d", len(claimed))
	}

	// Exactly the oldest 3 by (created_at, seq), in ascending order.
	wantOldest := []string{"oldn-1", "oldn-2", "oldn-3"}
	for i, rec := range claimed {
		if rec.ID() != wantOldest[i] {
			gotIDs := make([]string, len(claimed))
			for j, r := range claimed {
				gotIDs[j] = r.ID()
			}
			t.Fatalf("limited Claim must return the oldest-N by (created_at, seq) ascending: got %v, want %v",
				gotIDs, wantOldest)
		}
	}
	// Ascending (created_at, seq): each record is not-older than its predecessor,
	// and same-timestamp pairs are ordered by strictly increasing seq.
	for i := 1; i < len(claimed); i++ {
		prev, cur := claimed[i-1], claimed[i]
		if cur.CreatedAt().Before(prev.CreatedAt()) {
			t.Fatalf("claim order not ascending by created_at at %d: %v before %v",
				i, cur.CreatedAt(), prev.CreatedAt())
		}
		if cur.CreatedAt().Equal(prev.CreatedAt()) && cur.Seq() <= prev.Seq() {
			t.Fatalf("equal created_at at %d must order by strictly increasing seq: seq[%d]=%d seq[%d]=%d",
				i, i-1, prev.Seq(), i, cur.Seq())
		}
	}

	// Anti-starvation: the two NEWEST records were left unclaimed and remain
	// claimable. A second claim (same token version — the oldest 3 are now
	// claimed at this version and no longer claimable) drains exactly them,
	// proving the lexically-late envelope IDs were not starved.
	rest, err := store.Claim(ctx, "SESSION#sess-oldn", token, 3)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	wantRest := []string{"oldn-4", "oldn-5"}
	if len(rest) != len(wantRest) {
		gotIDs := make([]string, len(rest))
		for j, r := range rest {
			gotIDs[j] = r.ID()
		}
		t.Fatalf("second claim must drain the remaining newest records: got %v, want %v", gotIDs, wantRest)
	}
	for i, rec := range rest {
		if rec.ID() != wantRest[i] {
			t.Fatalf("second claim order at %d: got %q, want %q", i, rec.ID(), wantRest[i])
		}
	}
}

// testClaimVersionPreemptsPriorOwner proves version-monotonic reclaim: a
// record claimed under an older fencing version is claimable by a newer
// token (the prior owner's lease was preempted). This is version reclaim,
// not wall-clock stale reclaim — the time-based crash-recovery fallback is
// pinned separately by RunOutboxStaleReclaimTests for the backends that
// implement it.
func testClaimVersionPreemptsPriorOwner(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "recl-1", "env-recl-1", "bind-recl-1", "sess-recl", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	oldToken := persistence.LeaseToken{Version: 1, Owner: "owner-old"}
	claimed, err := store.Claim(ctx, "SESSION#sess-recl", oldToken, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v, len=%d", err, len(claimed))
	}

	newToken := persistence.LeaseToken{Version: 2, Owner: "owner-new"}
	reclaimed, err := store.Claim(ctx, "SESSION#sess-recl", newToken, 10)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("expected 1 reclaimed, got %d", len(reclaimed))
	}
	if reclaimed[0].ClaimedBy() != "owner-new" {
		t.Fatalf("claimedBy: got %q, want %q", reclaimed[0].ClaimedBy(), "owner-new")
	}
	if reclaimed[0].ClaimVersion() != 2 {
		t.Fatalf("claimVersion: got %d, want %d", reclaimed[0].ClaimVersion(), 2)
	}
	if reclaimed[0].ReplayCount() != 2 {
		t.Fatalf("replayCount: got %d, want %d", reclaimed[0].ReplayCount(), 2)
	}
}

func testCompleteWithValidToken(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "comp-1", "env-comp-1", "bind-comp-1", "sess-comp", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := persistence.LeaseToken{Version: 5, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, "SESSION#sess-comp", token, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v, len=%d", err, len(claimed))
	}

	if err := store.Complete(ctx, []string{"comp-1"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-comp", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after complete, got %d", len(pending))
	}
}

func testCompleteWithWrongToken(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "compw-1", "env-compw-1", "bind-compw-1", "sess-compw", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := persistence.LeaseToken{Version: 3, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-compw", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	wrongToken := persistence.LeaseToken{Version: 999, Owner: "owner-A"}
	err := store.Complete(ctx, []string{"compw-1"}, wrongToken)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

func testExpireMarksEligible(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	past := time.Now().Add(-1 * time.Hour)
	r := makeRecord(t, "exp-1", "env-exp-1", "bind-exp-1", "sess-exp", "route-1", past)
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	n, err := store.Expire(ctx, time.Now(), "SESSION#sess-exp")
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 expired, got %d", n)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-exp", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after expire, got %d", len(pending))
	}
}

// testExpireScopedToPartition proves Expire is partition-scoped (M1): a sweep
// for one partition must NOT expire another partition's pending-expired records,
// even though both are past their expiry. A drainer holding partition S1's lease
// must never destroy S2's records.
func testExpireScopedToPartition(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	past := time.Now().Add(-1 * time.Hour)
	r1 := makeRecord(t, "expscope-1", "env-expscope-1", "bind-expscope-1", "sess-expscope-1", "route-1", past)
	r2 := makeRecord(t, "expscope-2", "env-expscope-2", "bind-expscope-2", "sess-expscope-2", "route-1", past)
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r1, r2}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Sweep only partition 1.
	n, err := store.Expire(ctx, time.Now(), "SESSION#sess-expscope-1")
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 expired (only partition 1's record), got %d", n)
	}

	// Partition 1's record is gone from pending; partition 2's remains.
	p1, err := store.QueryPending(ctx, "SESSION#sess-expscope-1", 10)
	if err != nil {
		t.Fatalf("query p1: %v", err)
	}
	if len(p1) != 0 {
		t.Fatalf("expected 0 pending in swept partition 1, got %d", len(p1))
	}
	p2, err := store.QueryPending(ctx, "SESSION#sess-expscope-2", 10)
	if err != nil {
		t.Fatalf("query p2: %v", err)
	}
	if len(p2) != 1 {
		t.Fatalf("M1: unswept partition 2 must retain its pending record, got %d", len(p2))
	}
}

func testExpireSkipsCompleted(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	past := time.Now().Add(-1 * time.Hour)
	r := makeRecord(t, "expsk-1", "env-expsk-1", "bind-expsk-1", "sess-expsk", "route-1", past)
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-expsk", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.Complete(ctx, []string{"expsk-1"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}

	n, err := store.Expire(ctx, time.Now(), "SESSION#sess-expsk")
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 expired (already completed), got %d", n)
	}
}

func testQueryPendingOnlyPending(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()

	r1 := makeRecord(t, "qpo-1", "env-qpo-1", "bind-qpo-1", "sess-qpo", "route-1", time.Time{})
	r2 := makeRecord(t, "qpo-2", "env-qpo-2", "bind-qpo-2", "sess-qpo", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r1, r2}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-qpo", token, 1); err != nil {
		t.Fatalf("claim: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-qpo", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending (one is claimed), got %d", len(pending))
	}
}

func testQueryPendingRespectsPartitionKey(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()

	r1 := makeRecord(t, "qppk-1", "env-qppk-1", "bind-qppk-1", "sess-pk-A", "route-1", time.Time{})
	r2 := makeRecord(t, "qppk-2", "env-qppk-2", "bind-qppk-2", "sess-pk-B", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r1}); err != nil {
		t.Fatalf("persist r1: %v", err)
	}
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r2}); err != nil {
		t.Fatalf("persist r2: %v", err)
	}

	a, err := store.QueryPending(ctx, "SESSION#sess-pk-A", 10)
	if err != nil {
		t.Fatalf("query A: %v", err)
	}
	if len(a) != 1 || a[0].ID() != "qppk-1" {
		t.Fatalf("expected qppk-1 for session A, got %v", a)
	}

	b, err := store.QueryPending(ctx, "SESSION#sess-pk-B", 10)
	if err != nil {
		t.Fatalf("query B: %v", err)
	}
	if len(b) != 1 || b[0].ID() != "qppk-2" {
		t.Fatalf("expected qppk-2 for session B, got %v", b)
	}
}

func testFullLifecycle(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "lc-1", "env-lc-1", "bind-lc-1", "sess-lc", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, _ := store.QueryPending(ctx, "SESSION#sess-lc", 10)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	token := persistence.LeaseToken{Version: 10, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, "SESSION#sess-lc", token, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Status() != persistence.OutboxClaimed {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}

	if err := store.Complete(ctx, []string{"lc-1"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}

	pending, _ = store.QueryPending(ctx, "SESSION#sess-lc", 10)
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after complete, got %d", len(pending))
	}
}

func testIdempotentPersist(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord(t, "idem-1", "env-idem", "bind-idem", "sess-idem", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	r2 := makeRecord(t, "idem-2", "env-idem", "bind-idem", "sess-idem", "route-1", time.Time{})
	err := store.Persist(ctx, []*persistence.OutboxRecord{r2})
	if !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord on idempotent persist, got %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-idem", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected exactly 1 record (no duplicate), got %d", len(pending))
	}
}

func outboxCompleteAfterTokenChange(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	tok1 := persistence.LeaseToken{Version: 100, Owner: "owner-A"}
	tok2 := persistence.LeaseToken{Version: 200, Owner: "owner-B"}

	pk := persistence.OutboxPartitionKey("sess-catc", "")
	id := fmt.Sprintf("ox-catc-%d", time.Now().UnixNano())
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		EnvelopeID: "env-catc-1",
		SessionID:  "sess-catc",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-catc-1", Subject: "test"}),
	})
	if err != nil {
		t.Fatalf("new record: %v", err)
	}
	if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	claimed, claimErr := store.Claim(ctx, pk, tok1, 10)
	if claimErr != nil {
		t.Fatalf("claim: %v", claimErr)
	}
	if len(claimed) == 0 {
		t.Fatal("expected at least 1 claimed record")
	}

	_, claim2Err := store.Claim(ctx, pk, tok2, 10)
	if claim2Err != nil && !errors.Is(claim2Err, shared.ErrStaleFencingToken) {
		t.Fatalf("unexpected error on reclaim with higher token: %v", claim2Err)
	}

	completeErr := store.Complete(ctx, []string{id}, tok1)
	if completeErr == nil {
		t.Fatal("expected error when completing with old token after new claim")
	}
	if !errors.Is(completeErr, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", completeErr)
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return string(digits[n])
	}
	return itoa(n/10) + string(digits[n%10])
}

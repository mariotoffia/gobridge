package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// testPersistIdentityPartitionScoped asserts the ports.OutboxStore Persist
// idempotency identity is PARTITION-SCOPED — (partition key, EnvelopeID,
// BindingID) — not global. The SAME (EnvelopeID, BindingID) persisted under two
// DIFFERENT partitions (here two session identities) must yield TWO distinct,
// independently-claimable records; neither may be swallowed as a duplicate.
//
// This pins the contract: a GLOBAL (EnvelopeID, BindingID) identity silently
// loses a message re-persisted under a new partition after a session-identity
// change (acked upstream, delivered by nobody, recorded nowhere). DynamoDB is
// already partition-scoped and passes unchanged; the memory/SQLite backends
// historically enforced a global identity and fail this test until fixed.
func testPersistIdentityPartitionScoped(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()

	// Identical (envelope, binding) identity under two distinct session
	// partitions; record IDs differ (ID is the primary key).
	rA := makeRecord(t, "xpk-A", "env-xpk", "bind-xpk", "sess-xpk-A", "route-1", time.Time{})
	rB := makeRecord(t, "xpk-B", "env-xpk", "bind-xpk", "sess-xpk-B", "route-1", time.Time{})

	if err := store.Persist(ctx, []*persistence.OutboxRecord{rA}); err != nil {
		t.Fatalf("persist under partition A: %v", err)
	}
	// The cross-partition re-persist MUST NOT be swallowed as a duplicate.
	if err := store.Persist(ctx, []*persistence.OutboxRecord{rB}); err != nil {
		t.Fatalf("persist of same (envelope,binding) under a different partition "+
			"must succeed (identity is partition-scoped), got %v", err)
	}

	// Both records are independently present in their own partitions — proving
	// two DISTINCT claimable records, not one deduped away.
	pendA, err := store.QueryPending(ctx, "SESSION#sess-xpk-A", 10)
	if err != nil {
		t.Fatalf("query A: %v", err)
	}
	if len(pendA) != 1 || pendA[0].ID() != "xpk-A" {
		t.Fatalf("partition A must hold exactly [xpk-A], got %v", outboxIDs(pendA))
	}
	pendB, err := store.QueryPending(ctx, "SESSION#sess-xpk-B", 10)
	if err != nil {
		t.Fatalf("query B: %v", err)
	}
	if len(pendB) != 1 || pendB[0].ID() != "xpk-B" {
		t.Fatalf("partition B must hold exactly [xpk-B], got %v", outboxIDs(pendB))
	}

	// Each is separately claimable in its own partition.
	claimedA, err := store.Claim(ctx, "SESSION#sess-xpk-A",
		persistence.LeaseToken{Version: 1, Owner: "owner-A"}, 10)
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	claimedB, err := store.Claim(ctx, "SESSION#sess-xpk-B",
		persistence.LeaseToken{Version: 1, Owner: "owner-B"}, 10)
	if err != nil {
		t.Fatalf("claim B: %v", err)
	}
	if len(claimedA) != 1 || len(claimedB) != 1 {
		t.Fatalf("each partition must yield one claimable record: A=%d B=%d",
			len(claimedA), len(claimedB))
	}
	if claimedA[0].ID() == claimedB[0].ID() {
		t.Fatalf("cross-partition records must be distinct, both claimed as %q",
			claimedA[0].ID())
	}
}

// outboxIDs extracts record IDs for diagnostic failure messages.
func outboxIDs(recs []*persistence.OutboxRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID()
	}
	return out
}

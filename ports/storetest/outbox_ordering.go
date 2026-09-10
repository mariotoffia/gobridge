package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// Ordering-key head-of-line conformance. These pin the DURABLE half of per-key
// in-order delivery: the drainer can only sequence records inside one claimed
// batch, so the store must refuse to hand out a record whose older same-key
// sibling is still non-terminal. See the ordering-key head-of-line rule in the
// ports.OutboxStore Claim contract.

// makeOrderedRecord builds a record carrying a non-empty ordering key (or none
// when orderingKey is empty) with an explicit CreatedAt so the tests can place
// siblings in a known age order.
func makeOrderedRecord(
	t *testing.T,
	id, sessionID, orderingKey string,
	createdAt time.Time,
) *persistence.OutboxRecord {
	t.Helper()
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    "route-1",
		EnvelopeID: "env-" + id,
		BindingID:  "bind-" + id,
		SessionID:  sessionID,
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
		t.Fatalf("makeOrderedRecord %s: %v", id, err)
	}
	return rec
}

func claimedIDs(records []*persistence.OutboxRecord) []string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID())
	}
	return ids
}

func sameIDs(got []*persistence.OutboxRecord, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i, r := range got {
		if r.ID() != want[i] {
			return false
		}
	}
	return true
}

// testClaimBlocksYoungerSiblingOfStrandedHead proves a younger same-key record
// cannot overtake a head that is still Claimed from an earlier cycle. The head
// is left claimed under the SAME fencing version, which is exactly the state a
// failed Release or an abandoned batch leaves behind: it is not reclaimable
// (version-monotonic reclaim needs a strictly higher version) and it is not
// terminal, so the whole key must stall behind it until it resolves.
func testClaimBlocksYoungerSiblingOfStrandedHead(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	const (
		session = "sess-hol"
		pk      = "SESSION#" + session
		key     = "hol-key"
	)
	base := time.Now().Add(-time.Hour).UTC()

	head := makeOrderedRecord(t, "hol-head", session, key, base)
	tail := makeOrderedRecord(t, "hol-tail", session, key, base.Add(time.Second))
	loose := makeOrderedRecord(t, "hol-loose", session, "", base.Add(2*time.Second))
	for _, rec := range []*persistence.OutboxRecord{head, tail, loose} {
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", rec.ID(), err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-hol"}

	// Claim only the head; it stays Claimed for the rest of the test.
	first, err := store.Claim(ctx, pk, token, 1)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !sameIDs(first, "hol-head") {
		t.Fatalf("first claim: got %v, want [hol-head]", claimedIDs(first))
	}

	// The stranded head blocks its own key only: the keyless record is free.
	second, err := store.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !sameIDs(second, "hol-loose") {
		t.Fatalf("second claim: got %v, want [hol-loose] "+
			"(hol-tail must not overtake the still-claimed hol-head)", claimedIDs(second))
	}

	// Once the head reaches a terminal state the tail is claimable again.
	if err := store.Complete(ctx, []string{"hol-head"}, token); err != nil {
		t.Fatalf("complete head: %v", err)
	}
	third, err := store.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if !sameIDs(third, "hol-tail") {
		t.Fatalf("third claim: got %v, want [hol-tail] (head is terminal, the key is unblocked)",
			claimedIDs(third))
	}
}

// testClaimBlocksYoungerSiblingOfPendingHeadBeyondLimit proves the rule also
// holds when the head is merely OUTSIDE this claim's limit. A batch bounded to
// one record must return the oldest same-key record, never a younger sibling,
// so a `limit` smaller than a key's backlog cannot reorder it.
func testClaimBlocksYoungerSiblingOfPendingHeadBeyondLimit(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	const (
		session = "sess-hol-limit"
		pk      = "SESSION#" + session
		key     = "hol-limit-key"
	)
	base := time.Now().Add(-time.Hour).UTC()

	for i, id := range []string{"holl-a", "holl-b", "holl-c"} {
		rec := makeOrderedRecord(t, id, session, key, base.Add(time.Duration(i)*time.Second))
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", id, err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-hol-limit"}
	claimed, err := store.Claim(ctx, pk, token, 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !sameIDs(claimed, "holl-a") {
		t.Fatalf("claim: got %v, want [holl-a] (oldest same-key record first)", claimedIDs(claimed))
	}
}

// testClaimAllowsIndependentKeysPastStrandedHead proves the head-of-line rule
// is scoped to ONE ordering key: a stranded head on key A must not stall an
// unrelated key B. Otherwise a single stuck group would freeze the whole
// partition.
func testClaimAllowsIndependentKeysPastStrandedHead(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	const (
		session = "sess-hol-multi"
		pk      = "SESSION#" + session
	)
	base := time.Now().Add(-time.Hour).UTC()

	stuck := makeOrderedRecord(t, "holm-stuck", session, "key-A", base)
	blocked := makeOrderedRecord(t, "holm-blocked", session, "key-A", base.Add(time.Second))
	other := makeOrderedRecord(t, "holm-other", session, "key-B", base.Add(2*time.Second))
	for _, rec := range []*persistence.OutboxRecord{stuck, blocked, other} {
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", rec.ID(), err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-hol-multi"}
	if first, err := store.Claim(ctx, pk, token, 1); err != nil {
		t.Fatalf("first claim: %v", err)
	} else if !sameIDs(first, "holm-stuck") {
		t.Fatalf("first claim: got %v, want [holm-stuck]", claimedIDs(first))
	}

	second, err := store.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !sameIDs(second, "holm-other") {
		t.Fatalf("second claim: got %v, want [holm-other] "+
			"(key-A stalls behind its stranded head; key-B is independent)", claimedIDs(second))
	}
}

// testClaimReturnsWholeGroupWhenHeadIsClaimable proves the rule only defers
// work that has an UNRETURNABLE older sibling: when the head is claimed by the
// same batch, the whole group comes back together and in age order, so a
// healthy key drains at full batch width rather than one record per cycle.
func testClaimReturnsWholeGroupWhenHeadIsClaimable(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	const (
		session = "sess-hol-group"
		pk      = "SESSION#" + session
		key     = "hol-group-key"
	)
	base := time.Now().Add(-time.Hour).UTC()

	for i, id := range []string{"holg-1", "holg-2", "holg-3"} {
		rec := makeOrderedRecord(t, id, session, key, base.Add(time.Duration(i)*time.Second))
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", id, err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-hol-group"}
	claimed, err := store.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !sameIDs(claimed, "holg-1", "holg-2", "holg-3") {
		t.Fatalf("claim: got %v, want [holg-1 holg-2 holg-3]", claimedIDs(claimed))
	}
}

// RunOutboxClaimedDepthTests exercises the OPTIONAL
// ports.OutboxClaimedDepthReporter capability. It is a separate entry point (not
// part of RunOutboxStoreTests) because the capability is optional: an adapter
// opts in by calling this, exactly as it does for RunOutboxReleaseTests.
func RunOutboxClaimedDepthTests(t *testing.T, store ports.OutboxStore) {
	t.Helper()

	reporter, ok := store.(ports.OutboxClaimedDepthReporter)
	if !ok {
		t.Fatalf("store %T does not implement ports.OutboxClaimedDepthReporter", store)
	}
	t.Run("CountClaimedTracksLifecycle", func(t *testing.T) {
		testCountClaimedTracksLifecycle(t, store, reporter)
	})
	t.Run("CountClaimedScopedToPartition", func(t *testing.T) {
		testCountClaimedScopedToPartition(t, store, reporter)
	})
}

// testCountClaimedTracksLifecycle proves claimed depth is the signal that makes
// stranded work visible: it rises when a record is claimed and falls only when
// the record reaches a terminal state, so a record left Claimed by a failed
// release reports non-zero here while CountPending reports zero.
func testCountClaimedTracksLifecycle(
	t *testing.T,
	store ports.OutboxStore,
	reporter ports.OutboxClaimedDepthReporter,
) {
	ctx := context.Background()
	const (
		session = "sess-claimed-depth"
		pk      = "SESSION#" + session
	)
	base := time.Now().Add(-time.Hour).UTC()

	for i, id := range []string{"ccd-1", "ccd-2"} {
		rec := makeOrderedRecord(t, id, session, "", base.Add(time.Duration(i)*time.Second))
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", id, err)
		}
	}

	if n, err := reporter.CountClaimed(ctx, pk); err != nil || n != 0 {
		t.Fatalf("claimed depth before claim: got (%d, %v), want (0, nil)", n, err)
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-ccd"}
	if _, err := store.Claim(ctx, pk, token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n, err := reporter.CountClaimed(ctx, pk); err != nil || n != 2 {
		t.Fatalf("claimed depth after claim: got (%d, %v), want (2, nil)", n, err)
	}

	if err := store.Complete(ctx, []string{"ccd-1"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n, err := reporter.CountClaimed(ctx, pk); err != nil || n != 1 {
		t.Fatalf("claimed depth after complete: got (%d, %v), want (1, nil)", n, err)
	}
}

// testCountClaimedScopedToPartition proves the count never leaks across
// partitions: an owner's stranded-work gauge must describe only the partition
// it holds the lease for.
func testCountClaimedScopedToPartition(
	t *testing.T,
	store ports.OutboxStore,
	reporter ports.OutboxClaimedDepthReporter,
) {
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).UTC()

	mine := makeOrderedRecord(t, "ccdp-mine", "sess-ccdp-a", "", base)
	theirs := makeOrderedRecord(t, "ccdp-theirs", "sess-ccdp-b", "", base)
	for _, rec := range []*persistence.OutboxRecord{mine, theirs} {
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %s: %v", rec.ID(), err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-ccdp"}
	if _, err := store.Claim(ctx, "SESSION#sess-ccdp-a", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if n, err := reporter.CountClaimed(ctx, "SESSION#sess-ccdp-a"); err != nil || n != 1 {
		t.Fatalf("claimed depth for claimed partition: got (%d, %v), want (1, nil)", n, err)
	}
	if n, err := reporter.CountClaimed(ctx, "SESSION#sess-ccdp-b"); err != nil || n != 0 {
		t.Fatalf("claimed depth for untouched partition: got (%d, %v), want (0, nil)", n, err)
	}
}

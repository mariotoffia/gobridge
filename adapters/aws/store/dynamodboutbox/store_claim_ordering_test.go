package dynamodboutbox_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// TestClaimDeepBacklogSelectsOldestByAge is the regression: under a backlog
// deeper than the former 3×limit candidate window, Claim MUST return the
// oldest-by-age records first even when their envelope IDs sort AFTER the newer
// records in DynamoDB's SK order (OUTBOX#<envelope_id>#…, lexicographic by
// envelope ID).
//
// Adversarial construction: record i (i=0 is oldest) is stamped CreatedAt =
// base + i and EnvelopeID = env-%05d with (backlog-1-i), so the OLDEST record
// carries the LEXICOGRAPHICALLY-LARGEST envelope ID and therefore sorts LAST in
// SK order, while the NEWEST record sorts first.
//
// Counterfactual (why this has teeth): the former implementation gathered only
// the first claimRetentionFactor*limit (=3×limit) items in SK order and sorted
// just that window. With backlog > 3×limit the oldest records sit BEYOND that
// SK-order window, so they were never considered — the claim would return the
// NEWEST records first and the oldest starved indefinitely. Exhaustive paging
// considers every claimable record, so the oldest-N are the true oldest-N. This
// test fails on the pre-fix code (it asserts the oldest, latest-sorting record
// is claimed first) and passes on the fix.
func TestClaimDeepBacklogSelectsOldestByAge(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const (
		backlog = 60
		limit   = 5 // 3*limit = 15 << backlog, so the old window would miss the oldest
		session = "h1-deep-backlog"
	)
	partition := "SESSION#" + session
	base := time.Unix(1_700_000_000, 0).UTC()

	// Persist one record per call so each gets a distinct, increasing per-
	// partition Seq (persist order == age order), with adversarial envelope IDs.
	for i := 0; i < backlog; i++ {
		envID := fmt.Sprintf("env-%05d", backlog-1-i) // oldest -> largest ID (sorts last)
		rec := persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID:         fmt.Sprintf("rec-%05d", i),
			RouteID:    "route-1",
			EnvelopeID: envID,
			BindingID:  "bind-1",
			SessionID:  session,
			Address:    "test/topic",
			CreatedAt:  base.Add(time.Duration(i) * time.Second),
			Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: envID, Subject: "t"}),
		})
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-a"}

	// First claim: must be exactly the oldest `limit` records (rec-00000..rec-00004),
	// in ascending age order — the very records the old SK-order window starved.
	firstBatch, err := store.Claim(ctx, partition, token, limit)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(firstBatch) != limit {
		t.Fatalf("first claim: expected %d records, got %d", limit, len(firstBatch))
	}
	for i, rec := range firstBatch {
		want := fmt.Sprintf("rec-%05d", i)
		if rec.ID() != want {
			t.Fatalf("first claim position %d: expected oldest-by-age %q, got %q "+
				"(SK-order window would have returned the newest records first)", i, want, rec.ID())
		}
	}

	// Complete the first batch, then drain the rest and assert the FULL claim
	// order equals the age order across the deep backlog — no record starves.
	if err := store.Complete(ctx, idsOf(firstBatch), token); err != nil {
		t.Fatalf("complete first batch: %v", err)
	}

	claimedOrder := append([]string(nil), idsOf(firstBatch)...)
	for {
		batch, err := store.Claim(ctx, partition, token, limit)
		if err != nil {
			t.Fatalf("drain claim: %v", err)
		}
		if len(batch) == 0 {
			break
		}
		claimedOrder = append(claimedOrder, idsOf(batch)...)
		if err := store.Complete(ctx, idsOf(batch), token); err != nil {
			t.Fatalf("drain complete: %v", err)
		}
	}

	if len(claimedOrder) != backlog {
		t.Fatalf("expected to drain all %d records, drained %d", backlog, len(claimedOrder))
	}
	for i, id := range claimedOrder {
		want := fmt.Sprintf("rec-%05d", i)
		if id != want {
			t.Fatalf("drain order position %d: expected %q (age order), got %q", i, want, id)
		}
	}
}

func idsOf(recs []*persistence.OutboxRecord) []string {
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.ID()
	}
	return ids
}

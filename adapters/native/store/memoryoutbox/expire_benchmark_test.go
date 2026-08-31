package memoryoutbox_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// Expire runs on the drainer's own goroutine once per expire interval, ahead of
// the egress-readiness gate and the claim/send path, so its cost is paid by
// every drop-policy partition whether or not anything is eligible. The fencing
// check added to it is a map read plus a write under the mutex the sweep
// already holds, and these benchmarks exist to keep that true: the dominant
// cost must stay the O(records) partition scan, not the fence.
//
// The empty-partition case is the steady state — a sweep that finds nothing —
// and is therefore the one that must stay allocation-free.

func benchExpireStore(b *testing.B, records int, expired bool) (*memoryoutbox.Store, *clocktest.Fake) {
	b.Helper()
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	store := memoryoutbox.NewStore(memoryoutbox.WithClock(clk))
	ctx := context.Background()

	var expiresAt time.Time
	if expired {
		// Already past at construction, so every record is sweep-eligible.
		expiresAt = clk.Now().Add(-time.Hour)
	}
	batch := make([]*persistence.OutboxRecord, 0, records)
	for i := range records {
		id := fmt.Sprintf("bench-%d", i)
		batch = append(batch, persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID:         id,
			RouteID:    "route-bench",
			EnvelopeID: "env-" + id,
			BindingID:  "bind-bench",
			SessionID:  "sess-bench",
			Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-" + id, Subject: "bench"}),
			ExpiresAt:  expiresAt,
		}))
	}
	if records > 0 {
		if err := store.Persist(ctx, batch); err != nil {
			b.Fatalf("persist: %v", err)
		}
	}
	return store, clk
}

// The steady-state sweep: lease held, nothing eligible. This is what runs every
// expire interval on every drop-policy partition in a healthy bridge.
func BenchmarkStore_Expire_EmptyPartition(b *testing.B) {
	store, clk := benchExpireStore(b, 0, false)
	ctx := context.Background()
	token := persistence.LeaseToken{Version: 7, Owner: "owner-bench"}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.Expire(ctx, clk.Now(), "SESSION#sess-bench", token); err != nil {
			b.Fatalf("expire: %v", err)
		}
	}
}

// A populated partition with nothing eligible — the fence work is measured
// against the full partition scan it sits in front of.
func BenchmarkStore_Expire_ScanNoneEligible(b *testing.B) {
	store, clk := benchExpireStore(b, 1000, false)
	ctx := context.Background()
	token := persistence.LeaseToken{Version: 7, Owner: "owner-bench"}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.Expire(ctx, clk.Now(), "SESSION#sess-bench", token); err != nil {
			b.Fatalf("expire: %v", err)
		}
	}
}

// The rejection path: a preempted owner sweeping a partition whose fence has
// moved on. It must cost a fence comparison and nothing else — no partition
// scan — because a flapping standby can drive it every interval.
func BenchmarkStore_Expire_StaleTokenRejected(b *testing.B) {
	store, clk := benchExpireStore(b, 1000, true)
	ctx := context.Background()
	// A successor takes the partition at v9; the sweep below still carries v3.
	if _, err := store.Claim(ctx, "SESSION#sess-bench", persistence.LeaseToken{Version: 9, Owner: "owner-new"}, 0); err != nil {
		b.Fatalf("fence-advancing claim: %v", err)
	}
	stale := persistence.LeaseToken{Version: 3, Owner: "owner-old"}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.Expire(ctx, clk.Now(), "SESSION#sess-bench", stale); err == nil {
			b.Fatal("stale sweep must be rejected")
		}
	}
}

// The working sweep: a full partition of eligible records transitioned in one
// call. Only the first iteration transitions records (expiry is terminal), so
// this measures the eligible-scan cost with the fence in place rather than
// repeated state churn.
func BenchmarkStore_Expire_SweepEligible(b *testing.B) {
	store, clk := benchExpireStore(b, 1000, true)
	ctx := context.Background()
	token := persistence.LeaseToken{Version: 7, Owner: "owner-bench"}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.Expire(ctx, clk.Now(), "SESSION#sess-bench", token); err != nil {
			b.Fatalf("expire: %v", err)
		}
	}
}

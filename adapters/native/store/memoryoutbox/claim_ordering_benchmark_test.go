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

// Claim runs once per drain cycle on the drainer's only goroutine for the
// partition, so its cost is on the hot path for every message the outbox
// delivers. The ordering-key head-of-line rule adds a per-key blocker map built
// during the scan the claim already performs; these benchmarks exist to keep
// that true — the dominant cost must stay the O(records) partition scan and the
// sort, not the ordering bookkeeping.
//
// The keyless case is the one most deployments run and must stay closest to the
// pre-rule cost; the stranded-head case is the pathological one, where a blocked
// key forces every candidate through the blocker lookup.

const benchClaimLimit = 100

// benchClaimStore builds a partition of `records` pending records. keys is how
// many distinct ordering keys to spread them across; 0 means keyless. When
// strandHeads is true the oldest record of every key is left Claimed under the
// same fencing version, which is the stranded-head state the head-of-line rule
// has to detect.
func benchClaimStore(b *testing.B, records, keys int, strandHeads bool) *memoryoutbox.Store {
	b.Helper()
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	store := memoryoutbox.NewStore(memoryoutbox.WithClock(clk))
	ctx := context.Background()

	batch := make([]*persistence.OutboxRecord, 0, records)
	for i := range records {
		id := fmt.Sprintf("bench-%06d", i)
		key := ""
		if keys > 0 {
			key = fmt.Sprintf("key-%d", i%keys)
		}
		batch = append(batch, persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID:         id,
			RouteID:    "route-bench",
			EnvelopeID: "env-" + id,
			BindingID:  "bind-bench",
			SessionID:  "sess-bench",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:          "env-" + id,
				Subject:     "bench",
				OrderingKey: key,
			}),
			CreatedAt: clk.Now().Add(time.Duration(i) * time.Millisecond),
		}))
	}
	if err := store.Persist(ctx, batch); err != nil {
		b.Fatalf("persist: %v", err)
	}

	if strandHeads && keys > 0 {
		// One claim of exactly `keys` records takes the oldest record of every
		// key and never completes them, so each key now has a stranded head.
		if _, err := store.Claim(ctx, "SESSION#sess-bench",
			persistence.LeaseToken{Version: 1, Owner: "bench-owner"}, keys); err != nil {
			b.Fatalf("strand heads: %v", err)
		}
	}
	return store
}

func benchClaim(b *testing.B, records, keys int, strandHeads bool) {
	store := benchClaimStore(b, records, keys, strandHeads)
	ctx := context.Background()
	// A version above the stranded heads' claim version would reclaim them, so
	// the benchmark reuses version 1 — the state a live owner actually sees.
	token := persistence.LeaseToken{Version: 1, Owner: "bench-owner"}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.Claim(ctx, "SESSION#sess-bench", token, benchClaimLimit); err != nil {
			b.Fatalf("claim: %v", err)
		}
	}
}

// Keyless: the rule must cost effectively nothing when no record carries a key.
func BenchmarkClaim_Keyless_1k(b *testing.B)  { benchClaim(b, 1000, 0, false) }
func BenchmarkClaim_Keyless_10k(b *testing.B) { benchClaim(b, 10000, 0, false) }

// Every record keyed, nothing stranded: the blocker map stays empty, so this
// isolates the cost of reading a key per record.
func BenchmarkClaim_KeyedNoBlockers_1k(b *testing.B) { benchClaim(b, 1000, 50, false) }

// The pathological case: every key has a stranded head, so every candidate is
// tested against the blocker map and most are dropped.
func BenchmarkClaim_KeyedStrandedHeads_1k(b *testing.B)  { benchClaim(b, 1000, 50, true) }
func BenchmarkClaim_KeyedStrandedHeads_10k(b *testing.B) { benchClaim(b, 10000, 50, true) }

// A partition whose records all share ONE key and whose head is stranded: the
// whole backlog is blocked, which is the cheapest possible claim (nothing is
// returned) and must not degrade into per-candidate work proportional to the
// blocker set.
func BenchmarkClaim_SingleBlockedKey_10k(b *testing.B) { benchClaim(b, 10000, 1, true) }

// CountClaimed backs the stranded-work gauge and runs once per drain cycle, so
// it shares the hot path with Claim.
func BenchmarkCountClaimed_10k(b *testing.B) {
	store := benchClaimStore(b, 10000, 50, true)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.CountClaimed(ctx, "SESSION#sess-bench"); err != nil {
			b.Fatalf("count claimed: %v", err)
		}
	}
}

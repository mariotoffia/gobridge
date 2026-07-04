package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// RunOutboxFirstAttemptTests pins the replay-budget first-attempt contract that
// every OutboxStore backend must honour:
//
//   - Claim stamps FirstAttemptedAt exactly once, at the first claim, equal to
//     ClaimedAt at that moment.
//   - A later release+reclaim or a stale/version reclaim advances ClaimedAt but
//     leaves FirstAttemptedAt frozen at the first claim.
//   - A never-claimed record round-trips a ZERO first attempt through persist /
//     QueryPending — the marshal path must never now-stamp a legacy zero.
//
// The caller constructs the store with a fake clock and supplies advance to
// move the SAME fake clock forward (TESTS.md: no time.Sleep, ever). The clock
// must be injected because SQLite truncates timestamps to milliseconds; a real
// fast wall clock would make the "ClaimedAt advanced past FirstAttemptedAt"
// assertions flaky.
//
// The first subtest uses the optional OutboxReleaser capability; a store that
// does not implement it is skipped for that case only.
func RunOutboxFirstAttemptTests(t *testing.T, store ports.OutboxStore, advance func(time.Duration)) {
	t.Helper()

	t.Run("FirstAttemptStampedOnClaimAndStable", func(t *testing.T) {
		testFirstAttemptStampedOnClaimAndStable(t, store, advance)
	})
	t.Run("FirstAttemptSurvivesStaleReclaim", func(t *testing.T) {
		testFirstAttemptSurvivesStaleReclaim(t, store, advance)
	})
	t.Run("LegacyZeroFirstAttemptRoundTrips", func(t *testing.T) {
		testLegacyZeroFirstAttemptRoundTrips(t, store)
	})
}

// testFirstAttemptStampedOnClaimAndStable: pending → zero; first claim stamps
// FirstAttemptedAt == ClaimedAt; release + reclaim at a later clock keeps
// FirstAttemptedAt frozen while ClaimedAt advances.
func testFirstAttemptStampedOnClaimAndStable(t *testing.T, store ports.OutboxStore, advance func(time.Duration)) {
	ctx := context.Background()
	pk := "SESSION#sess-fa"
	token := persistence.LeaseToken{Version: 1, Owner: "owner-fa"}

	r := makeRecord(t, "fa-1", "env-fa-1", "bind-fa-1", "sess-fa", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Before any claim the record carries a zero first attempt.
	pending, err := store.QueryPending(ctx, pk, 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	got := findRecord(pending, "fa-1")
	if got == nil {
		t.Fatalf("record fa-1 not returned by QueryPending")
	}
	if !got.FirstAttemptedAt().IsZero() {
		t.Fatalf("pending record must have a zero first attempt, got %v", got.FirstAttemptedAt())
	}

	// First claim stamps the first-attempt instant, equal to ClaimedAt.
	claimed, err := store.Claim(ctx, pk, token, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v, len=%d", err, len(claimed))
	}
	first := claimed[0].FirstAttemptedAt()
	if first.IsZero() {
		t.Fatalf("first claim must stamp a non-zero FirstAttemptedAt")
	}
	if !first.Equal(claimed[0].ClaimedAt()) {
		t.Fatalf("on the first claim FirstAttemptedAt %v must equal ClaimedAt %v", first, claimed[0].ClaimedAt())
	}

	// Release + reclaim at a later clock: the first attempt is frozen, the
	// claim timestamp moves forward.
	releaser, ok := store.(ports.OutboxReleaser)
	if !ok {
		t.Skip("store does not implement ports.OutboxReleaser; release-path reclaim not exercised")
	}
	if err := releaser.Release(ctx, []string{"fa-1"}, token); err != nil {
		t.Fatalf("release: %v", err)
	}

	advance(time.Minute)

	reclaimed, err := store.Claim(ctx, pk, token, 10)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaim after release: err=%v, len=%d", err, len(reclaimed))
	}
	if !reclaimed[0].FirstAttemptedAt().Equal(first) {
		t.Fatalf("FirstAttemptedAt moved on reclaim: got %v, want %v (frozen)", reclaimed[0].FirstAttemptedAt(), first)
	}
	if !reclaimed[0].ClaimedAt().After(first) {
		t.Fatalf("ClaimedAt must advance past the first attempt on reclaim: claimedAt=%v, first=%v", reclaimed[0].ClaimedAt(), first)
	}

	if err := store.Complete(ctx, []string{"fa-1"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

// testFirstAttemptSurvivesStaleReclaim: a reclaim by a NEWER fencing token
// (version preemption, supported by every backend) advances ClaimedAt and
// ReplayCount but leaves FirstAttemptedAt frozen at the first claim.
func testFirstAttemptSurvivesStaleReclaim(t *testing.T, store ports.OutboxStore, advance func(time.Duration)) {
	ctx := context.Background()
	pk := "SESSION#sess-fa2"
	tokenV1 := persistence.LeaseToken{Version: 1, Owner: "owner-a"}
	tokenV2 := persistence.LeaseToken{Version: 2, Owner: "owner-b"}

	r := makeRecord(t, "fa2-1", "env-fa2-1", "bind-fa2-1", "sess-fa2", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	claimed, err := store.Claim(ctx, pk, tokenV1, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v, len=%d", err, len(claimed))
	}
	first := claimed[0].FirstAttemptedAt()
	if first.IsZero() {
		t.Fatalf("first claim must stamp a non-zero FirstAttemptedAt")
	}

	// A newer fencing token preempts the stranded claim.
	advance(time.Minute)

	reclaimed, err := store.Claim(ctx, pk, tokenV2, 10)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("version-preemption reclaim: err=%v, len=%d", err, len(reclaimed))
	}
	if !reclaimed[0].FirstAttemptedAt().Equal(first) {
		t.Fatalf("FirstAttemptedAt moved on stale reclaim: got %v, want %v (frozen)", reclaimed[0].FirstAttemptedAt(), first)
	}
	if !reclaimed[0].ClaimedAt().After(first) {
		t.Fatalf("ClaimedAt must advance on reclaim: claimedAt=%v, first=%v", reclaimed[0].ClaimedAt(), first)
	}
	if reclaimed[0].ReplayCount() != 2 {
		t.Fatalf("ReplayCount after reclaim: got %d, want 2", reclaimed[0].ReplayCount())
	}

	if err := store.Complete(ctx, []string{"fa2-1"}, tokenV2); err != nil {
		t.Fatalf("complete after reclaim: %v", err)
	}
}

// testLegacyZeroFirstAttemptRoundTrips: a persisted-but-never-claimed record
// round-trips a ZERO first attempt through QueryPending. The marshal/unmarshal
// path must never now-stamp a legacy zero (that would make every legacy record
// look freshly attempted and mis-drive the replay budget).
func testLegacyZeroFirstAttemptRoundTrips(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	pk := "SESSION#sess-fa3"

	r := makeRecord(t, "fa3-1", "env-fa3-1", "bind-fa3-1", "sess-fa3", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := store.QueryPending(ctx, pk, 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	got := findRecord(pending, "fa3-1")
	if got == nil {
		t.Fatalf("record fa3-1 not returned by QueryPending")
	}
	if !got.FirstAttemptedAt().IsZero() {
		t.Fatalf("never-claimed record must round-trip a zero first attempt, got %v", got.FirstAttemptedAt())
	}
}

// findRecord returns the record with the given ID from a slice, or nil.
func findRecord(records []*persistence.OutboxRecord, id string) *persistence.OutboxRecord {
	for _, r := range records {
		if r.ID() == id {
			return r
		}
	}
	return nil
}

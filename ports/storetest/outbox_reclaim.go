package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// RunOutboxStaleReclaimTests pins the OPTIONAL wall-clock stale-claim
// reclaim behaviour (crash recovery for an owner that died between Claim and
// Complete/Release without a fencing-version bump).
//
// The caller constructs the store with a fake clock and a known stale-claim
// duration, passes that duration as staleAfter, and supplies advance to move
// the SAME fake clock forward (TESTS.md: no time.Sleep, ever). Backends that
// are strictly version-only (memoryoutbox) must NOT run this suite.
func RunOutboxStaleReclaimTests(t *testing.T, store ports.OutboxStore, staleAfter time.Duration, advance func(time.Duration)) {
	t.Helper()

	t.Run("StaleClaimReclaimedBySameToken", func(t *testing.T) {
		testStaleClaimReclaimedBySameToken(t, store, staleAfter, advance)
	})
	t.Run("FreshClaimNotReclaimed", func(t *testing.T) {
		testFreshClaimNotReclaimed(t, store, staleAfter, advance)
	})
}

// testStaleClaimReclaimedBySameToken: a claim stranded past the stale window
// becomes claimable again by the SAME owner+version — no version bump needed.
// The reclaim increments ReplayCount so the drainer's poison gate still sees
// every attempt.
func testStaleClaimReclaimedBySameToken(t *testing.T, store ports.OutboxStore, staleAfter time.Duration, advance func(time.Duration)) {
	ctx := context.Background()
	pk := "SESSION#sess-stale"
	token := persistence.LeaseToken{Version: 1, Owner: "owner-stale"}

	r := makeRecord(t, "stale-1", "env-stale-1", "bind-stale-1", "sess-stale", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	claimed, err := store.Claim(ctx, pk, token, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v, len=%d", err, len(claimed))
	}

	// The owner crashes without completing. Past the stale window the same
	// token must reclaim the stranded record.
	advance(staleAfter + time.Second)

	reclaimed, err := store.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("stale reclaim: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("expected 1 stale-reclaimed record, got %d", len(reclaimed))
	}
	if reclaimed[0].ID() != "stale-1" {
		t.Fatalf("reclaimed ID: got %q, want %q", reclaimed[0].ID(), "stale-1")
	}
	if reclaimed[0].ReplayCount() != 2 {
		t.Fatalf("replayCount after stale reclaim: got %d, want 2", reclaimed[0].ReplayCount())
	}

	// The reclaiming owner holds a live claim and can complete it.
	if err := store.Complete(ctx, []string{"stale-1"}, token); err != nil {
		t.Fatalf("complete after stale reclaim: %v", err)
	}
}

// testFreshClaimNotReclaimed: within the stale window a claimed record is NOT
// claimable by the same token — stale reclaim must not double-deliver work
// whose owner is merely slow, only work whose claim has aged past the window.
func testFreshClaimNotReclaimed(t *testing.T, store ports.OutboxStore, staleAfter time.Duration, advance func(time.Duration)) {
	ctx := context.Background()
	pk := "SESSION#sess-fresh"
	token := persistence.LeaseToken{Version: 1, Owner: "owner-fresh"}

	r := makeRecord(t, "fresh-1", "env-fresh-1", "bind-fresh-1", "sess-fresh", "route-1", time.Time{})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	claimed, err := store.Claim(ctx, pk, token, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v, len=%d", err, len(claimed))
	}

	// Well inside the stale window: the claim is fresh, so nothing is
	// claimable.
	advance(staleAfter / 2)

	again, err := store.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("in-window claim: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("expected 0 records inside the stale window, got %d", len(again))
	}

	if err := store.Complete(ctx, []string{"fresh-1"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

package memoryoutbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

// Compile-time assertion that the in-memory store implements the optional
// OutboxReleaser capability the drainer type-asserts for the A4 transient-
// failure fast path. It lives in the test file because the production
// package satisfies its ports structurally (no ports import) per
// .go-arch-lint.yml; only memorydlq carries an in-package ports assertion.
var _ ports.OutboxReleaser = (*memoryoutbox.Store)(nil)

// Validates the in-memory outbox store against the shared conformance suite.
func TestOutboxStoreConformance(t *testing.T) {
	store := memoryoutbox.NewStore()
	storetest.RunOutboxStoreTests(t, store)
}

// Validates the optional fast-release capability against the shared
// conformance suite so release fencing matches SQLite/DynamoDB.
func TestOutboxReleaseConformance(t *testing.T) {
	store := memoryoutbox.NewStore()
	storetest.RunOutboxReleaseTests(t, store)
}

// Validates the replay-budget first-attempt contract against the shared
// conformance suite, driven by a fake clock (TESTS.md: no time.Sleep). The
// memory store holds full aggregates, so Claim's stamp-once flows through the
// snapshot round-trip end-to-end.
func TestOutboxFirstAttemptConformance(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	store := memoryoutbox.NewStore(memoryoutbox.WithClock(clk))
	storetest.RunOutboxFirstAttemptTests(t, store, clk.Advance)
}

// newClockStore builds a memory store driven by an injected fake clock so
// the Release tests are deterministic and free of any wall-clock or sleep
// dependence.
func newClockStore(t *testing.T) *memoryoutbox.Store {
	t.Helper()
	clk := clocktest.NewAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	return memoryoutbox.NewStore(memoryoutbox.WithClock(clk))
}

func mustPersist(t *testing.T, store *memoryoutbox.Store, id, sessionID string) {
	t.Helper()
	rec := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    "route-1",
		EnvelopeID: "env-" + id,
		BindingID:  "bind-" + id,
		SessionID:  sessionID,
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-" + id, Subject: "test"}),
	})
	if err := store.Persist(context.Background(), []*persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist %s: %v", id, err)
	}
}

// TestRelease_AllowsSameOwnerRetryAfterTransientFailure proves the A4
// fast path: a live owner returns a transiently-failed claimed record to
// pending via Release and re-claims it on the next drain with the SAME
// token version — no fencing-version bump and no wall-clock stale-claim
// wait. replay_count is unchanged by Release but advances on the re-Claim.
func TestRelease_AllowsSameOwnerRetryAfterTransientFailure(t *testing.T) {
	store := newClockStore(t)
	ctx := context.Background()
	const sessionID = "sess-rel"
	pk := persistence.OutboxPartitionKey(sessionID, "")
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	mustPersist(t, store, "rel-1", sessionID)

	claimed, err := store.Claim(ctx, pk, token, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v len=%d", err, len(claimed))
	}
	if claimed[0].ReplayCount() != 1 {
		t.Fatalf("replayCount after first claim = %d, want 1", claimed[0].ReplayCount())
	}

	if err := store.Release(ctx, []string{"rel-1"}, token); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Released → pending again; replay_count untouched by Release.
	pending, err := store.QueryPending(ctx, pk, 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID() != "rel-1" {
		t.Fatalf("expected rel-1 pending after release, got %v", pending)
	}
	if pending[0].Status() != persistence.OutboxPending {
		t.Fatalf("status after release = %q, want pending", pending[0].Status())
	}
	if pending[0].ReplayCount() != 1 {
		t.Fatalf("replayCount after release = %d, want 1 (unchanged)", pending[0].ReplayCount())
	}

	// The SAME owner at the SAME version re-claims it; only now does
	// replay_count advance.
	reclaimed, err := store.Claim(ctx, pk, token, 10)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("re-claim: err=%v len=%d", err, len(reclaimed))
	}
	if reclaimed[0].ReplayCount() != 2 {
		t.Fatalf("replayCount after re-claim = %d, want 2", reclaimed[0].ReplayCount())
	}
}

// TestRelease_FencingRejectsMismatch verifies Release enforces the
// owner+version+status fence (identical to Complete): a wrong owner, a
// never-claimed (pending) record, and an already-completed record are all
// rejected with shared.ErrStaleFencingToken.
func TestRelease_FencingRejectsMismatch(t *testing.T) {
	store := newClockStore(t)
	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	t.Run("never_claimed_pending_rejected", func(t *testing.T) {
		mustPersist(t, store, "relf-pending", "sess-relf-p")
		err := store.Release(ctx, []string{"relf-pending"}, token)
		if !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("got %v, want ErrStaleFencingToken", err)
		}
	})

	t.Run("wrong_owner_rejected", func(t *testing.T) {
		mustPersist(t, store, "relf-owner", "sess-relf-o")
		if _, err := store.Claim(ctx, "SESSION#sess-relf-o", token, 10); err != nil {
			t.Fatalf("claim: %v", err)
		}
		wrongOwner := persistence.LeaseToken{Version: 1, Owner: "owner-B"}
		err := store.Release(ctx, []string{"relf-owner"}, wrongOwner)
		if !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("got %v, want ErrStaleFencingToken", err)
		}
	})

	t.Run("already_completed_rejected", func(t *testing.T) {
		mustPersist(t, store, "relf-done", "sess-relf-d")
		if _, err := store.Claim(ctx, "SESSION#sess-relf-d", token, 10); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := store.Complete(ctx, []string{"relf-done"}, token); err != nil {
			t.Fatalf("complete: %v", err)
		}
		err := store.Release(ctx, []string{"relf-done"}, token)
		if !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("got %v, want ErrStaleFencingToken", err)
		}
	})
}

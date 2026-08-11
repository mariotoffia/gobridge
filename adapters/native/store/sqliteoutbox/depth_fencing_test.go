package sqliteoutbox_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// invalidTokens enumerates the three shapes an unusable fencing token can take:
// a full zero value, an empty owner, and a zero version. None may ever mutate
// the outbox (persistence.LeaseToken.Valid == false for all three).
var invalidTokens = []struct {
	name  string
	token persistence.LeaseToken
}{
	{"zero_value", persistence.LeaseToken{}},
	{"empty_owner", persistence.LeaseToken{Version: 1}},
	{"zero_version", persistence.LeaseToken{Owner: "owner-A"}},
}

// newMemClockStore opens an in-memory SQLite outbox driven by a fixed fake
// clock so the depth/fencing tests are deterministic and sleep-free.
func newMemClockStore(t *testing.T) *sqliteoutbox.Store {
	t.Helper()
	clk := clocktest.NewAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	s, err := sqliteoutbox.NewStore(":memory:", sqliteoutbox.WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustPersistPending seeds one pending record under sessionID.
func mustPersistPending(t *testing.T, store *sqliteoutbox.Store, id, sessionID string) {
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

// TestClaim_RejectsInvalidTokenAndDoesNotMutate is the facade fencing guard
// on Claim: an invalid token is rejected with shared.ErrStaleFencingToken
// BEFORE any SQL runs, claims zero records, and leaves the pending row untouched
// so a valid token still wins it. Mutation-verify: delete the guard in
// Store.Claim and this test fails (raw SQL claims the row under an empty owner).
func TestClaim_RejectsInvalidTokenAndDoesNotMutate(t *testing.T) {
	store := newMemClockStore(t)
	ctx := context.Background()
	const sessionID = "sess-claimguard"
	pk := persistence.OutboxPartitionKey(sessionID, "")
	mustPersistPending(t, store, "cg-1", sessionID)

	for _, tc := range invalidTokens {
		claimed, err := store.Claim(ctx, pk, tc.token, 10)
		if !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("%s: got err %v, want ErrStaleFencingToken", tc.name, err)
		}
		if len(claimed) != 0 {
			t.Fatalf("%s: invalid token claimed %d records, want 0", tc.name, len(claimed))
		}
	}

	valid := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, pk, valid, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID() != "cg-1" {
		t.Fatalf("valid claim after rejected tokens: err=%v claimed=%d", err, len(claimed))
	}
}

// TestComplete_RejectsInvalidTokenAndDoesNotMutate is the facade fencing
// guard on Complete: an invalid token is rejected with
// shared.ErrStaleFencingToken and leaves the row claimed so its rightful owner
// can still complete it.
func TestComplete_RejectsInvalidTokenAndDoesNotMutate(t *testing.T) {
	store := newMemClockStore(t)
	ctx := context.Background()
	const sessionID = "sess-completeguard"
	pk := persistence.OutboxPartitionKey(sessionID, "")
	owner := persistence.LeaseToken{Version: 3, Owner: "owner-A"}
	mustPersistPending(t, store, "compg-1", sessionID)
	if _, err := store.Claim(ctx, pk, owner, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	for _, tc := range invalidTokens {
		if err := store.Complete(ctx, []string{"compg-1"}, tc.token); !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("%s: got err %v, want ErrStaleFencingToken", tc.name, err)
		}
	}

	if err := store.Complete(ctx, []string{"compg-1"}, owner); err != nil {
		t.Fatalf("rightful complete after rejected tokens: %v", err)
	}
}

// TestRelease_RejectsInvalidTokenAndDoesNotMutate is the facade fencing
// guard on Release: an invalid token is rejected with
// shared.ErrStaleFencingToken and leaves the row claimed (not pending).
func TestRelease_RejectsInvalidTokenAndDoesNotMutate(t *testing.T) {
	store := newMemClockStore(t)
	ctx := context.Background()
	const sessionID = "sess-releaseguard"
	pk := persistence.OutboxPartitionKey(sessionID, "")
	owner := persistence.LeaseToken{Version: 2, Owner: "owner-A"}
	mustPersistPending(t, store, "relg-1", sessionID)
	if _, err := store.Claim(ctx, pk, owner, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	for _, tc := range invalidTokens {
		if err := store.Release(ctx, []string{"relg-1"}, tc.token); !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("%s: got err %v, want ErrStaleFencingToken", tc.name, err)
		}
	}

	pending, err := store.QueryPending(ctx, pk, 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("row became pending after rejected release, want still claimed; got %d", len(pending))
	}
	if err := store.Release(ctx, []string{"relg-1"}, owner); err != nil {
		t.Fatalf("rightful release after rejected tokens: %v", err)
	}
}

// TestCountPending_ReturnsTrueBacklogNotBatchSize proves CountPending reports
// the real pending backlog via an indexed COUNT(*), not the claim batch size.
// Seed a deep backlog, claim a small batch (which leaves the pending set), and
// CountPending must return the remaining backlog — never the batch count.
//
// The seed is kept around a thousand (not 9500) because the production Persist
// insert carries an O(n) MAX(seq) subselect per row (a documented ponytail in
// acl_query.go), making a single-partition bulk seed O(n^2); ~1000 rows already
// dwarfs the 100-record batch and keeps the test fast under -race. The
// in-memory backend (O(1) seq) exercises the full 9500.
func TestCountPending_ReturnsTrueBacklogNotBatchSize(t *testing.T) {
	store := newMemClockStore(t)
	ctx := context.Background()
	const sessionID = "sess-backlog"
	pk := persistence.OutboxPartitionKey(sessionID, "")

	const seeded = 1000
	if err := store.Persist(ctx, seedPending(t, sessionID, seeded)); err != nil {
		t.Fatalf("persist batch: %v", err)
	}

	got, err := store.CountPending(ctx, pk)
	if err != nil {
		t.Fatalf("count before claim: %v", err)
	}
	if got != seeded {
		t.Fatalf("CountPending before claim = %d, want %d", got, seeded)
	}

	const claimN = 100
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, pk, token, claimN)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != claimN {
		t.Fatalf("claimed %d records, want %d", len(claimed), claimN)
	}

	got, err = store.CountPending(ctx, pk)
	if err != nil {
		t.Fatalf("count after claim: %v", err)
	}
	if want := seeded - claimN; got != want {
		t.Fatalf("CountPending after claim = %d, want %d (true backlog)", got, want)
	}
	if got == len(claimed) {
		t.Fatalf("CountPending returned the claim batch size (%d), not the backlog", got)
	}
}

// TestCountPending_PartitionFiltering proves CountPending scopes to a partition
// and, with an empty key, counts across all partitions.
func TestCountPending_PartitionFiltering(t *testing.T) {
	store := newMemClockStore(t)
	ctx := context.Background()

	if err := store.Persist(ctx, seedPending(t, "sess-A", 30)); err != nil {
		t.Fatalf("persist A: %v", err)
	}
	if err := store.Persist(ctx, seedPending(t, "sess-B", 12)); err != nil {
		t.Fatalf("persist B: %v", err)
	}

	pkA := persistence.OutboxPartitionKey("sess-A", "")
	pkB := persistence.OutboxPartitionKey("sess-B", "")

	if got, err := store.CountPending(ctx, pkA); err != nil || got != 30 {
		t.Fatalf("CountPending(A) = %d, err=%v, want 30", got, err)
	}
	if got, err := store.CountPending(ctx, pkB); err != nil || got != 12 {
		t.Fatalf("CountPending(B) = %d, err=%v, want 12", got, err)
	}
	if got, err := store.CountPending(ctx, ""); err != nil || got != 42 {
		t.Fatalf("CountPending(all) = %d, err=%v, want 42", got, err)
	}
}

// seedPending builds n distinct pending records under sessionID (same
// SESSION#<sessionID> partition, unique binding/envelope per record).
func seedPending(t *testing.T, sessionID string, n int) []*persistence.OutboxRecord {
	t.Helper()
	out := make([]*persistence.OutboxRecord, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%d", sessionID, i)
		out = append(out, persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID:         id,
			RouteID:    "route-1",
			EnvelopeID: "env-" + id,
			BindingID:  "bind-" + id,
			SessionID:  sessionID,
			Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-" + id, Subject: "test"}),
		}))
	}
	return out
}

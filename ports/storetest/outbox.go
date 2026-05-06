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

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func makeRecord(id, envelopeID, bindingID, sessionID, routeID string, expiresAt time.Time) domain.OutboxRecord {
	return domain.OutboxRecord{
		ID:         id,
		RouteID:    routeID,
		EnvelopeID: envelopeID,
		BindingID:  bindingID,
		SessionID:  sessionID,
		Address:    "test/topic",
		Envelope: messaging.Envelope{
			ID:      envelopeID,
			Subject: "test-subject",
			Payload: []byte(`{"data":"value"}`),
			Headers: map[string]any{"key": "val"},
		},
		DispatchHeaders: map[string]any{"dispatch": "header"},
		ExpiresAt:       expiresAt,
	}
}

// RunOutboxStoreTests executes the full conformance suite against the given
// OutboxStore. The caller is responsible for creating and closing the store.
func RunOutboxStoreTests(t *testing.T, store ports.OutboxStore) {
	t.Helper()

	t.Run("PersistAndQueryPending", func(t *testing.T) {
		testPersistAndQueryPending(t, store)
	})
	t.Run("PersistDuplicate", func(t *testing.T) {
		testPersistDuplicate(t, store)
	})
	t.Run("PersistFanOut", func(t *testing.T) {
		testPersistFanOut(t, store)
	})
	t.Run("ClaimTransitionsStatus", func(t *testing.T) {
		testClaimTransitionsStatus(t, store)
	})
	t.Run("ClaimRespectsLimit", func(t *testing.T) {
		testClaimRespectsLimit(t, store)
	})
	t.Run("ClaimReclaimsStale", func(t *testing.T) {
		testClaimReclaimsStale(t, store)
	})
	t.Run("CompleteWithValidToken", func(t *testing.T) {
		testCompleteWithValidToken(t, store)
	})
	t.Run("CompleteWithWrongToken", func(t *testing.T) {
		testCompleteWithWrongToken(t, store)
	})
	t.Run("ExpireMarksEligible", func(t *testing.T) {
		testExpireMarksEligible(t, store)
	})
	t.Run("ExpireSkipsCompleted", func(t *testing.T) {
		testExpireSkipsCompleted(t, store)
	})
	t.Run("QueryPendingOnlyPending", func(t *testing.T) {
		testQueryPendingOnlyPending(t, store)
	})
	t.Run("QueryPendingRespectsPartitionKey", func(t *testing.T) {
		testQueryPendingRespectsPartitionKey(t, store)
	})
	t.Run("FullLifecycle", func(t *testing.T) {
		testFullLifecycle(t, store)
	})
	t.Run("IdempotentPersist", func(t *testing.T) {
		testIdempotentPersist(t, store)
	})
	t.Run("CompleteAfterTokenChange", func(t *testing.T) {
		outboxCompleteAfterTokenChange(t, store)
	})
}

func testPersistAndQueryPending(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord("pq-1", "env-pq-1", "bind-pq-1", "sess-pq", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	results, err := store.QueryPending(ctx, "SESSION#sess-pq", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 record, got %d", len(results))
	}
	if results[0].ID != "pq-1" {
		t.Fatalf("id: got %q, want %q", results[0].ID, "pq-1")
	}
	if results[0].Status != domain.OutboxPending {
		t.Fatalf("status: got %q, want %q", results[0].Status, domain.OutboxPending)
	}
	if results[0].Envelope.ID != "env-pq-1" {
		t.Fatalf("envelope.ID: got %q, want %q", results[0].Envelope.ID, "env-pq-1")
	}
	if string(results[0].Envelope.Payload) != `{"data":"value"}` {
		t.Fatalf("payload mismatch: %q", results[0].Envelope.Payload)
	}
}

func testPersistDuplicate(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord("dup-1", "env-dup", "bind-dup", "sess-dup", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	r2 := makeRecord("dup-2", "env-dup", "bind-dup", "sess-dup", "route-1", time.Time{})
	err := store.Persist(ctx, []domain.OutboxRecord{r2})
	if !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord, got %v", err)
	}
}

func testPersistFanOut(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	records := []domain.OutboxRecord{
		makeRecord("fo-1", "env-fo", "bind-A", "sess-fo", "route-1", time.Time{}),
		makeRecord("fo-2", "env-fo", "bind-B", "sess-fo", "route-1", time.Time{}),
		makeRecord("fo-3", "env-fo", "bind-C", "sess-fo", "route-1", time.Time{}),
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
	r := makeRecord("cl-1", "env-cl-1", "bind-cl-1", "sess-cl", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := domain.LeaseToken{Version: 1, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, "SESSION#sess-cl", "owner-A", token, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}
	if claimed[0].Status != domain.OutboxClaimed {
		t.Fatalf("status: got %q, want %q", claimed[0].Status, domain.OutboxClaimed)
	}
	if claimed[0].ClaimedBy != "owner-A" {
		t.Fatalf("claimedBy: got %q, want %q", claimed[0].ClaimedBy, "owner-A")
	}
	if claimed[0].ClaimVersion != 1 {
		t.Fatalf("claimVersion: got %d, want %d", claimed[0].ClaimVersion, 1)
	}
	if claimed[0].ReplayCount != 1 {
		t.Fatalf("replayCount: got %d, want %d", claimed[0].ReplayCount, 1)
	}
}

func testClaimRespectsLimit(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		r := makeRecord(
			"clim-"+itoa(i), "env-clim-"+itoa(i), "bind-clim-"+itoa(i),
			"sess-clim", "route-1", time.Time{},
		)
		if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}

	token := domain.LeaseToken{Version: 1, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, "SESSION#sess-clim", "owner-A", token, 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("expected 3, got %d", len(claimed))
	}
}

func testClaimReclaimsStale(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord("recl-1", "env-recl-1", "bind-recl-1", "sess-recl", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	oldToken := domain.LeaseToken{Version: 1, Owner: "owner-old"}
	claimed, err := store.Claim(ctx, "SESSION#sess-recl", "owner-old", oldToken, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v, len=%d", err, len(claimed))
	}

	newToken := domain.LeaseToken{Version: 2, Owner: "owner-new"}
	reclaimed, err := store.Claim(ctx, "SESSION#sess-recl", "owner-new", newToken, 10)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("expected 1 reclaimed, got %d", len(reclaimed))
	}
	if reclaimed[0].ClaimedBy != "owner-new" {
		t.Fatalf("claimedBy: got %q, want %q", reclaimed[0].ClaimedBy, "owner-new")
	}
	if reclaimed[0].ClaimVersion != 2 {
		t.Fatalf("claimVersion: got %d, want %d", reclaimed[0].ClaimVersion, 2)
	}
	if reclaimed[0].ReplayCount != 2 {
		t.Fatalf("replayCount: got %d, want %d", reclaimed[0].ReplayCount, 2)
	}
}

func testCompleteWithValidToken(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord("comp-1", "env-comp-1", "bind-comp-1", "sess-comp", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := domain.LeaseToken{Version: 5, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, "SESSION#sess-comp", "owner-A", token, 10)
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
	r := makeRecord("compw-1", "env-compw-1", "bind-compw-1", "sess-compw", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := domain.LeaseToken{Version: 3, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-compw", "owner-A", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	wrongToken := domain.LeaseToken{Version: 999, Owner: "owner-A"}
	err := store.Complete(ctx, []string{"compw-1"}, wrongToken)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

func testExpireMarksEligible(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	past := time.Now().Add(-1 * time.Hour)
	r := makeRecord("exp-1", "env-exp-1", "bind-exp-1", "sess-exp", "route-1", past)
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	n, err := store.Expire(ctx, time.Now())
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

func testExpireSkipsCompleted(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	past := time.Now().Add(-1 * time.Hour)
	r := makeRecord("expsk-1", "env-expsk-1", "bind-expsk-1", "sess-expsk", "route-1", past)
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := domain.LeaseToken{Version: 1, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-expsk", "owner-A", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.Complete(ctx, []string{"expsk-1"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}

	n, err := store.Expire(ctx, time.Now())
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 expired (already completed), got %d", n)
	}
}

func testQueryPendingOnlyPending(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()

	r1 := makeRecord("qpo-1", "env-qpo-1", "bind-qpo-1", "sess-qpo", "route-1", time.Time{})
	r2 := makeRecord("qpo-2", "env-qpo-2", "bind-qpo-2", "sess-qpo", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r1, r2}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := domain.LeaseToken{Version: 1, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-qpo", "owner-A", token, 1); err != nil {
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

	r1 := makeRecord("qppk-1", "env-qppk-1", "bind-qppk-1", "sess-pk-A", "route-1", time.Time{})
	r2 := makeRecord("qppk-2", "env-qppk-2", "bind-qppk-2", "sess-pk-B", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r1}); err != nil {
		t.Fatalf("persist r1: %v", err)
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r2}); err != nil {
		t.Fatalf("persist r2: %v", err)
	}

	a, err := store.QueryPending(ctx, "SESSION#sess-pk-A", 10)
	if err != nil {
		t.Fatalf("query A: %v", err)
	}
	if len(a) != 1 || a[0].ID != "qppk-1" {
		t.Fatalf("expected qppk-1 for session A, got %v", a)
	}

	b, err := store.QueryPending(ctx, "SESSION#sess-pk-B", 10)
	if err != nil {
		t.Fatalf("query B: %v", err)
	}
	if len(b) != 1 || b[0].ID != "qppk-2" {
		t.Fatalf("expected qppk-2 for session B, got %v", b)
	}
}

func testFullLifecycle(t *testing.T, store ports.OutboxStore) {
	ctx := context.Background()
	r := makeRecord("lc-1", "env-lc-1", "bind-lc-1", "sess-lc", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, _ := store.QueryPending(ctx, "SESSION#sess-lc", 10)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	token := domain.LeaseToken{Version: 10, Owner: "owner-A"}
	claimed, err := store.Claim(ctx, "SESSION#sess-lc", "owner-A", token, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Status != domain.OutboxClaimed {
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
	r := makeRecord("idem-1", "env-idem", "bind-idem", "sess-idem", "route-1", time.Time{})
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	r2 := makeRecord("idem-2", "env-idem", "bind-idem", "sess-idem", "route-1", time.Time{})
	err := store.Persist(ctx, []domain.OutboxRecord{r2})
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
	tok1 := domain.LeaseToken{Version: 100, Owner: "owner-A"}
	tok2 := domain.LeaseToken{Version: 200, Owner: "owner-B"}

	pk := domain.OutboxPartitionKey("sess-catc", "")
	recs := []domain.OutboxRecord{
		{
			ID:         fmt.Sprintf("ox-catc-%d", time.Now().UnixNano()),
			EnvelopeID: "env-catc-1",
			SessionID:  "sess-catc",
			Envelope:   messaging.Envelope{ID: "env-catc-1", Subject: "test"},
			Status:     domain.OutboxPending,
		},
	}
	if err := store.Persist(ctx, recs); err != nil {
		t.Fatalf("persist: %v", err)
	}

	claimed, err := store.Claim(ctx, pk, "owner-A", tok1, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) == 0 {
		t.Fatal("expected at least 1 claimed record")
	}

	_, err = store.Claim(ctx, pk, "owner-B", tok2, 10)
	if err != nil && !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("unexpected error on reclaim with higher token: %v", err)
	}

	err = store.Complete(ctx, []string{recs[0].ID}, tok1)
	if err == nil {
		t.Fatal("expected error when completing with old token after new claim")
	}
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken, got %v", err)
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return string(digits[n])
	}
	return itoa(n/10) + string(digits[n%10])
}

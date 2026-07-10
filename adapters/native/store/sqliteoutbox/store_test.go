package sqliteoutbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

// Compile-time assertion that the SQLite store implements the optional
// OutboxReleaser capability the drainer type-asserts for the A4 transient-
// failure fast path. It lives in the test file because the production
// package satisfies its ports structurally (no ports import) per
// .go-arch-lint.yml; only memorydlq carries an in-package ports assertion.
var _ ports.OutboxReleaser = (*sqliteoutbox.Store)(nil)

// Compile-time assertion that the SQLite store implements the optional
// OutboxDepthReporter capability the drainer type-asserts to emit the true
// pending backlog (shared.MetricOutboxDepth). Same test-package placement
// rationale as the OutboxReleaser assertion above (no production ports import
// per .go-arch-lint.yml).
var _ ports.OutboxDepthReporter = (*sqliteoutbox.Store)(nil)

func newTempStore(t *testing.T) *sqliteoutbox.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "outbox.db")
	s, err := sqliteoutbox.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Validates the SQLite outbox store against the shared conformance suite.
func TestOutboxStoreConformance(t *testing.T) {
	store := newTempStore(t)
	storetest.RunOutboxStoreTests(t, store)
}

// Validates the optional fast-release capability against the shared
// conformance suite so release fencing matches memory/DynamoDB.
func TestOutboxReleaseConformance(t *testing.T) {
	store := newTempStore(t)
	storetest.RunOutboxReleaseTests(t, store)
}

// Validates the in-memory SQLite outbox store against the shared conformance suite.
func TestInMemoryMode(t *testing.T) {
	s, err := sqliteoutbox.NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore(:memory:): %v", err)
	}
	defer func() { _ = s.Close() }()
	storetest.RunOutboxStoreTests(t, s)
}

// Validates the wall-clock stale-claim reclaim behaviour against the shared
// conformance suite, driven by a fake clock (TESTS.md: no time.Sleep).
func TestOutboxStaleReclaimConformance(t *testing.T) {
	const stale = 5 * time.Minute
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s, err := sqliteoutbox.NewStore(":memory:",
		sqliteoutbox.WithClock(clk),
		sqliteoutbox.WithStaleClaimDuration(stale))
	if err != nil {
		t.Fatalf("NewStore(:memory:): %v", err)
	}
	defer func() { _ = s.Close() }()
	storetest.RunOutboxStaleReclaimTests(t, s, stale, clk.Advance)
}

// Validates the replay-budget first-attempt contract against the shared
// conformance suite, driven by a fake clock (TESTS.md: no time.Sleep). Proves
// the store-side CASE-WHEN stamp and the millis row round-trip.
func TestOutboxFirstAttemptConformance(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	s, err := sqliteoutbox.NewStore(":memory:", sqliteoutbox.WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore(:memory:): %v", err)
	}
	defer func() { _ = s.Close() }()
	storetest.RunOutboxFirstAttemptTests(t, s, clk.Advance)
}

// Verifies persisted outbox records survive closing and reopening the database file.
func TestDurability_CloseAndReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "durable.db")

	s1, err := sqliteoutbox.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}

	ctx := context.Background()
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "dur-1",
		RouteID:    "route-1",
		EnvelopeID: "env-dur-1",
		BindingID:  "bind-dur-1",
		SessionID:  "sess-dur",
		Address:    "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-dur-1",
			Subject: "test",
			Payload: []byte("durable payload"),
		}),
	})
	if err := s1.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	s2, err := sqliteoutbox.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	pending, err := s2.QueryPending(ctx, "SESSION#sess-dur", 10)
	if err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 record after reopen, got %d", len(pending))
	}
	if pending[0].ID() != "dur-1" {
		t.Fatalf("id: got %q, want %q", pending[0].ID(), "dur-1")
	}
	if string(pending[0].Snapshot().Payload()) != "durable payload" {
		t.Fatalf("payload mismatch: %q", pending[0].Snapshot().Payload())
	}
}

// Verifies the database file remains after Close following a persist.
func TestTempFileCleanup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cleanup.db")

	s, err := sqliteoutbox.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	ctx := context.Background()
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "tmp-1",
		RouteID:    "route-1",
		EnvelopeID: "env-tmp-1",
		BindingID:  "bind-tmp-1",
		SessionID:  "sess-tmp",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-tmp-1", Subject: "test"}),
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatal("db file should exist after close")
	}
}

// Verifies dispatch headers round-trip through persist and query.
func TestDispatchHeadersRoundTrip(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "hdr-1",
		RouteID:    "route-1",
		EnvelopeID: "env-hdr-1",
		BindingID:  "bind-hdr-1",
		SessionID:  "sess-hdr",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-hdr-1", Subject: "test"}),
		DispatchHeaders: map[string]any{
			"x-custom":  "value",
			"x-numeric": float64(42),
		},
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := s.QueryPending(ctx, "SESSION#sess-hdr", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if v, ok := pending[0].DispatchHeaders()["x-custom"]; !ok || v != "value" {
		t.Fatalf("dispatch header x-custom: %v", pending[0].DispatchHeaders())
	}
}

// Verifies WithClock controls default CreatedAt timestamps for persisted records.
func TestWithClockControlsCreatedAt(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2026, 5, 4, 12, 30, 45, 123000000, time.UTC))
	s, err := sqliteoutbox.NewStore(":memory:", sqliteoutbox.WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore(:memory:): %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "clk-1",
		RouteID:    "route-1",
		EnvelopeID: "env-clk-1",
		BindingID:  "bind-clk-1",
		SessionID:  "sess-clk",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-clk-1", Subject: "test"}),
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := s.QueryPending(ctx, "SESSION#sess-clk", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if !pending[0].CreatedAt().Equal(clk.Now()) {
		t.Fatalf("createdAt: got %v, want %v", pending[0].CreatedAt(), clk.Now())
	}
}

// Verifies ExpiresAt round-trips through persist and query.
func TestExpiresAtRoundTrip(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "exprt-1",
		RouteID:    "route-1",
		EnvelopeID: "env-exprt-1",
		BindingID:  "bind-exprt-1",
		SessionID:  "sess-exprt",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-exprt-1", Subject: "test"}),
		ExpiresAt:  expiry,
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := s.QueryPending(ctx, "SESSION#sess-exprt", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if !pending[0].ExpiresAt().Equal(expiry) {
		t.Fatalf("expiresAt: got %v, want %v", pending[0].ExpiresAt(), expiry)
	}
}

// persistSQLite is a small helper that inserts a single pending record
// under the given session for the Release tests.
func persistSQLite(t *testing.T, s *sqliteoutbox.Store, id, sessionID string) {
	t.Helper()
	rec := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    "route-1",
		EnvelopeID: "env-" + id,
		BindingID:  "bind-" + id,
		SessionID:  sessionID,
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-" + id, Subject: "test"}),
	})
	if err := s.Persist(context.Background(), []*persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist %s: %v", id, err)
	}
}

// TestRelease_AllowsSameOwnerRetryAfterTransientFailure proves the A4
// fast path on the SQLite backend: a live owner returns a
// transiently-failed claimed record to pending via Release and re-claims
// it on the next drain with the SAME token version — no fencing-version
// bump and no wall-clock stale-claim wait. replay_count is unchanged by
// Release but advances on the re-Claim.
func TestRelease_AllowsSameOwnerRetryAfterTransientFailure(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()
	const sessionID = "sess-rel"
	pk := persistence.OutboxPartitionKey(sessionID, "")
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	persistSQLite(t, s, "rel-1", sessionID)

	claimed, err := s.Claim(ctx, pk, token, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v len=%d", err, len(claimed))
	}
	if claimed[0].ReplayCount() != 1 {
		t.Fatalf("replayCount after first claim = %d, want 1", claimed[0].ReplayCount())
	}

	if err := s.Release(ctx, []string{"rel-1"}, token); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Released → pending again; replay_count untouched by Release.
	pending, err := s.QueryPending(ctx, pk, 10)
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
	reclaimed, err := s.Claim(ctx, pk, token, 10)
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
	s := newTempStore(t)
	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	t.Run("never_claimed_pending_rejected", func(t *testing.T) {
		persistSQLite(t, s, "relf-pending", "sess-relf-p")
		err := s.Release(ctx, []string{"relf-pending"}, token)
		if !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("got %v, want ErrStaleFencingToken", err)
		}
	})

	t.Run("wrong_owner_rejected", func(t *testing.T) {
		persistSQLite(t, s, "relf-owner", "sess-relf-o")
		if _, err := s.Claim(ctx, "SESSION#sess-relf-o", token, 10); err != nil {
			t.Fatalf("claim: %v", err)
		}
		wrongOwner := persistence.LeaseToken{Version: 1, Owner: "owner-B"}
		err := s.Release(ctx, []string{"relf-owner"}, wrongOwner)
		if !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("got %v, want ErrStaleFencingToken", err)
		}
	})

	t.Run("already_completed_rejected", func(t *testing.T) {
		persistSQLite(t, s, "relf-done", "sess-relf-d")
		if _, err := s.Claim(ctx, "SESSION#sess-relf-d", token, 10); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.Complete(ctx, []string{"relf-done"}, token); err != nil {
			t.Fatalf("complete: %v", err)
		}
		err := s.Release(ctx, []string{"relf-done"}, token)
		if !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("got %v, want ErrStaleFencingToken", err)
		}
	})
}

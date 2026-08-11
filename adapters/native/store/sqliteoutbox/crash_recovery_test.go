package sqliteoutbox_test

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

func persistCrashRec(t *testing.T, s *sqliteoutbox.Store, id, sessionID string) {
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

// TestClaimedRecordCrashRecovery_StaleReclaim is the regression: a record
// claimed by an owner that then crashes (never Complete/Release) must be
// recoverable after the process restarts. It reproduces the single-instance
// SQLite-outbox + memory-lease topology where the reset lease lands on the
// SAME fencing version, so the version-only reclaim rule can never fire and
// the record would be stranded forever. With a stale-claim duration the
// stranded claim is reclaimed once the wall-clock window elapses. The paired
// TestClaimedRecordCrashRecovery_VersionOnlyStrands documents the pre-fix
// (version-only) behaviour this recovers from.
func TestClaimedRecordCrashRecovery_StaleReclaim(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "outbox.db")
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	const stale = 30 * time.Second
	ctx := context.Background()
	pk := persistence.OutboxPartitionKey("sess-crash", "")
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	// Owner claims a record then "crashes" without completing or releasing.
	s1, err := sqliteoutbox.NewStore(dbPath,
		sqliteoutbox.WithClock(clk),
		sqliteoutbox.WithStaleClaimDuration(stale))
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	persistCrashRec(t, s1, "crash-1", "sess-crash")
	claimed, err := s1.Claim(ctx, pk, token, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v len=%d", err, len(claimed))
	}
	if err := s1.Close(); err != nil { // simulate crash / process exit
		t.Fatalf("close 1: %v", err)
	}

	// Restart against the same file. The recovering owner re-drains with the
	// same token version. Within the stale window the stranded claim is not
	// yet re-claimable.
	s2, err := sqliteoutbox.NewStore(dbPath,
		sqliteoutbox.WithClock(clk),
		sqliteoutbox.WithStaleClaimDuration(stale))
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	early, err := s2.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("early re-claim: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("expected no reclaim within stale window, got %d", len(early))
	}

	// Once the stale window elapses the stranded claim is recovered.
	clk.Advance(stale + time.Second)
	recovered, err := s2.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("stale re-claim: %v", err)
	}
	if len(recovered) != 1 || recovered[0].ID() != "crash-1" {
		t.Fatalf("expected crash-1 recovered, got %v", recovered)
	}
}

// TestClaimedRecordCrashRecovery_VersionOnlyStrands pins the historical
// version-only behaviour (no stale-claim duration configured): a claimed
// record whose owner crashed and re-drains at the same version is NEVER
// reclaimed no matter how much wall-clock time passes. This is exactly the
// durability gap; the stale-claim path above is the fix.
func TestClaimedRecordCrashRecovery_VersionOnlyStrands(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "outbox.db")
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	ctx := context.Background()
	pk := persistence.OutboxPartitionKey("sess-strand", "")
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	s, err := sqliteoutbox.NewStore(dbPath, sqliteoutbox.WithClock(clk)) // version-only
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	persistCrashRec(t, s, "strand-1", "sess-strand")
	if _, err := s.Claim(ctx, pk, token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	clk.Advance(24 * time.Hour)
	again, err := s.Claim(ctx, pk, token, 10)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("version-only store must strand the claim, got %d reclaimed", len(again))
	}
}

// TestConcurrentClaims_NoDuplicateNoLoss is the regression: concurrent
// claimers sharing a fencing version against a single file-backed partition
// must partition the pending records with no duplicate and no lost row, and
// no SQLITE_BUSY failure. The single writer connection plus the guarded claim
// UPDATE are what make this hold.
func TestConcurrentClaims_NoDuplicateNoLoss(t *testing.T) {
	dir := t.TempDir()
	s, err := sqliteoutbox.NewStore(filepath.Join(dir, "conc.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	const total = 200
	const sessionID = "sess-conc"
	pk := persistence.OutboxPartitionKey(sessionID, "")
	for i := 0; i < total; i++ {
		persistCrashRec(t, s, "conc-"+strconv.Itoa(i), sessionID)
	}

	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	var mu sync.Mutex
	seen := make(map[string]int, total)
	errCh := make(chan error, 16)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				recs, err := s.Claim(ctx, pk, token, 7)
				if err != nil {
					errCh <- err
					return
				}
				if len(recs) == 0 {
					return
				}
				mu.Lock()
				for _, r := range recs {
					seen[r.ID()]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent claim error: %v", err)
	}

	if len(seen) != total {
		t.Fatalf("expected %d distinct records claimed, got %d", total, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("record %s claimed %d times, want exactly 1", id, n)
		}
	}
}

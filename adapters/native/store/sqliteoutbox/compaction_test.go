package sqliteoutbox

// Internal tests for retention compaction (WithRetention) and the additive
// schema migrations (seq column, fence updated_at). They live in the
// production package because they must inspect raw row counts through the
// session handle — the public port surface deliberately cannot observe
// terminal rows.

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

func mustRecordAt(t *testing.T, id, sess string, createdAt, expiresAt time.Time) *persistence.OutboxRecord {
	t.Helper()
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         id,
		RouteID:    "route-1",
		EnvelopeID: "env-" + id,
		BindingID:  "bind-" + id,
		SessionID:  sess,
		Address:    "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-" + id,
			Subject: "test-subject",
			Payload: []byte(`{}`),
		}),
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("new record %s: %v", id, err)
	}
	return rec
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.sess.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Completed rows older than the retention window are physically deleted by
// the compaction pass piggybacked on Complete; fresh terminal rows survive.
func TestRetentionCompactsCompletedRows(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clk := clocktest.NewAt(t0)
	const retention = 30 * time.Minute

	s, err := NewStore(":memory:", WithClock(clk), WithRetention(retention))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	// Complete record A at t0.
	if err := s.Persist(ctx, []*persistence.OutboxRecord{mustRecordAt(t, "ra", "sess-cmp", t0, time.Time{})}); err != nil {
		t.Fatalf("persist A: %v", err)
	}
	if _, err := s.Claim(ctx, "SESSION#sess-cmp", token, 10); err != nil {
		t.Fatalf("claim A: %v", err)
	}
	if err := s.Complete(ctx, []string{"ra"}, token); err != nil {
		t.Fatalf("complete A: %v", err)
	}
	if got := countRows(t, s, "outbox"); got != 1 {
		t.Fatalf("A must survive compaction inside the retention window, rows=%d", got)
	}

	// One retention window later, complete record B: the piggybacked pass
	// must delete A (terminal since t0) and keep B.
	clk.Advance(retention + time.Minute)
	if err := s.Persist(ctx, []*persistence.OutboxRecord{mustRecordAt(t, "rb", "sess-cmp", clk.Now(), time.Time{})}); err != nil {
		t.Fatalf("persist B: %v", err)
	}
	if _, err := s.Claim(ctx, "SESSION#sess-cmp", token, 10); err != nil {
		t.Fatalf("claim B: %v", err)
	}
	if err := s.Complete(ctx, []string{"rb"}, token); err != nil {
		t.Fatalf("complete B: %v", err)
	}

	if got := countRows(t, s, "outbox"); got != 1 {
		t.Fatalf("expected exactly 1 row after compaction (B), got %d", got)
	}
	var id string
	if err := s.sess.db.QueryRow("SELECT id FROM outbox").Scan(&id); err != nil {
		t.Fatalf("select survivor: %v", err)
	}
	if id != "rb" {
		t.Fatalf("survivor: got %q, want %q", id, "rb")
	}
}

// Expired rows past retention are deleted on the pass piggybacked on Expire,
// and fence rows of partitions untouched for max(retention, 30d) are dropped.
func TestRetentionCompactsExpiredRowsAndStaleFences(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clk := clocktest.NewAt(t0)
	const retention = 30 * time.Minute

	s, err := NewStore(":memory:", WithClock(clk), WithRetention(retention))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}

	// A no-op claim creates a fence row for an ephemeral partition.
	if _, err := s.Claim(ctx, "SESSION#sess-ephemeral", token, 10); err != nil {
		t.Fatalf("noop claim: %v", err)
	}
	if got := countRows(t, s, "outbox_partition_fence"); got != 1 {
		t.Fatalf("expected 1 fence row, got %d", got)
	}

	// A record that expires at t0+1m.
	if err := s.Persist(ctx, []*persistence.OutboxRecord{
		mustRecordAt(t, "re", "sess-exp", t0, t0.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Mark it expired, then jump past 31 days so both the expired row
	// (terminal since t0+1m) and the ephemeral partition's untouched fence
	// (30d floor) are stale.
	if _, err := s.Expire(ctx, t0.Add(2*time.Minute), "SESSION#sess-exp", token); err != nil {
		t.Fatalf("expire: %v", err)
	}
	clk.Advance(31 * 24 * time.Hour)
	if _, err := s.Expire(ctx, clk.Now(), "SESSION#sess-exp", token); err != nil {
		t.Fatalf("expire trigger: %v", err)
	}

	if got := countRows(t, s, "outbox"); got != 0 {
		t.Fatalf("expected expired row compacted, rows=%d", got)
	}
	// Expire is lease-fenced, so sweeping a partition RAISES and therefore
	// touches its fence. Only the ephemeral partition — claimed once at t0 and
	// never swept since — is stale enough to compact; the partition this test
	// just swept is by definition live and must keep its fence, otherwise a
	// preempted owner could reclaim the partition after compaction.
	fences := fencePartitions(t, s)
	if len(fences) != 1 || fences[0] != "SESSION#sess-exp" {
		t.Fatalf("expected only the just-swept partition's fence to survive compaction, got %v", fences)
	}
}

// fencePartitions lists the partition keys that still hold a fence row.
func fencePartitions(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.sess.db.Query("SELECT partition_key FROM outbox_partition_fence ORDER BY partition_key")
	if err != nil {
		t.Fatalf("query fences: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			t.Fatalf("scan fence: %v", err)
		}
		out = append(out, pk)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate fences: %v", err)
	}
	return out
}

// WithRetention(<=0) disables compaction entirely: terminal rows and fences
// are never deleted (the historical behaviour).
func TestRetentionDisabledKeepsTerminalRows(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clk := clocktest.NewAt(t0)

	s, err := NewStore(":memory:", WithClock(clk), WithRetention(0))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	token := persistence.LeaseToken{Version: 1, Owner: "owner-A"}
	if err := s.Persist(ctx, []*persistence.OutboxRecord{mustRecordAt(t, "rk", "sess-keep", t0, time.Time{})}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := s.Claim(ctx, "SESSION#sess-keep", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.Complete(ctx, []string{"rk"}, token); err != nil {
		t.Fatalf("complete: %v", err)
	}

	clk.Advance(365 * 24 * time.Hour)
	if _, err := s.Expire(ctx, clk.Now(), "SESSION#sess-keep", token); err != nil {
		t.Fatalf("expire trigger: %v", err)
	}

	if got := countRows(t, s, "outbox"); got != 1 {
		t.Fatalf("retention disabled: expected completed row kept, rows=%d", got)
	}
	if got := countRows(t, s, "outbox_partition_fence"); got != 1 {
		t.Fatalf("retention disabled: expected fence kept, fences=%d", got)
	}
}

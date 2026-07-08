package memoryoutbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// persistOne persists a single record with a derived envelope/binding identity
// under sessionID and fails the test on error. It is white-box so eviction and
// dedup effects can be observed directly through recordCount/dedupCount.
func persistOne(t *testing.T, s *Store, id, sessionID string) {
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

func recordCount(s *Store) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func dedupCount(s *Store) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.dedup)
}

func hasRecord(s *Store, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.records[id]
	return ok
}

// driveToCompleted persists id under sessionID and drives it to the terminal
// completed state at the clock's current instant.
func driveToCompleted(t *testing.T, s *Store, clk *clocktest.Fake, id, sessionID string) {
	t.Helper()
	ctx := context.Background()
	persistOne(t, s, id, sessionID)
	pk := persistence.OutboxPartitionKey(sessionID, "bind-"+id)
	token := persistence.LeaseToken{Version: 1, Owner: "owner"}
	if _, err := s.Claim(ctx, pk, token, 10); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	if err := s.Complete(ctx, []string{id}, token); err != nil {
		t.Fatalf("complete %s: %v", id, err)
	}
}

// TestEvictsTerminalAfterRetention proves the LOW growth-bound fix: a terminal
// (completed) record whose CompletedAt precedes now-retention is dropped by the
// piggybacked sweep, while a freshly-persisted pending record survives.
func TestEvictsTerminalAfterRetention(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	s := NewStore(WithClock(clk)) // default retention 1h

	driveToCompleted(t, s, clk, "done", "sess-A")
	if got := recordCount(s); got != 1 {
		t.Fatalf("records after complete = %d, want 1", got)
	}

	// Advance well past the retention window, then trigger a sweep via a
	// Persist in a different partition.
	clk.Advance(2 * time.Hour)
	persistOne(t, s, "fresh", "sess-B")

	if hasRecord(s, "done") {
		t.Fatalf("terminal record 'done' should have been evicted after retention")
	}
	if !hasRecord(s, "fresh") {
		t.Fatalf("pending record 'fresh' must survive eviction")
	}
	if got := recordCount(s); got != 1 {
		t.Fatalf("records after eviction = %d, want 1 (only 'fresh')", got)
	}
	// The evicted record's dedup identity is released alongside it.
	if got := dedupCount(s); got != 1 {
		t.Fatalf("dedup entries after eviction = %d, want 1", got)
	}
}

// TestPendingAndClaimedNeverEvicted proves eviction is loss-free: neither a
// pending nor a claimed (in-flight) record is ever dropped, no matter how far
// the clock advances past the retention window.
func TestPendingAndClaimedNeverEvicted(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	s := NewStore(WithClock(clk))
	ctx := context.Background()

	persistOne(t, s, "pending", "sess-P")
	persistOne(t, s, "claimed", "sess-C")
	pk := persistence.OutboxPartitionKey("sess-C", "bind-claimed")
	if _, err := s.Claim(ctx, pk, persistence.LeaseToken{Version: 1, Owner: "o"}, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	clk.Advance(1000 * time.Hour)
	persistOne(t, s, "trigger", "sess-T") // fires the sweep at now+1000h

	if !hasRecord(s, "pending") {
		t.Fatalf("pending record must never be evicted")
	}
	if !hasRecord(s, "claimed") {
		t.Fatalf("claimed (in-flight) record must never be evicted")
	}
}

// TestRetentionDisabledKeepsTerminal proves WithRetention(<=0) restores the
// historical unbounded behaviour: terminal records are retained indefinitely.
func TestRetentionDisabledKeepsTerminal(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	s := NewStore(WithClock(clk), WithRetention(-1))

	driveToCompleted(t, s, clk, "done", "sess-A")
	clk.Advance(1000 * time.Hour)
	persistOne(t, s, "fresh", "sess-B")

	if !hasRecord(s, "done") {
		t.Fatalf("with retention disabled, terminal record must be retained")
	}
	if got := recordCount(s); got != 2 {
		t.Fatalf("records with retention disabled = %d, want 2", got)
	}
}

// TestEvictionReleasesDedupIdentity proves that dropping a terminal record also
// frees its (partition, envelope, binding) dedup slot, so an identical identity
// re-persisted after the window is admitted as new rather than swallowed as a
// duplicate — the documented retention/redelivery-window tradeoff.
func TestEvictionReleasesDedupIdentity(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	s := NewStore(WithClock(clk))
	ctx := context.Background()

	driveToCompleted(t, s, clk, "dup", "sess-A")

	// Before eviction the identity is still deduped.
	rec := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "dup", RouteID: "route-1", EnvelopeID: "env-dup", BindingID: "bind-dup",
		SessionID: "sess-A",
		Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-dup", Subject: "test"}),
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{rec}); !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("pre-eviction re-persist: got %v, want ErrDuplicateRecord", err)
	}

	// Advance past retention and sweep.
	clk.Advance(2 * time.Hour)
	persistOne(t, s, "trigger", "sess-Z")
	if hasRecord(s, "dup") {
		t.Fatalf("terminal 'dup' should be evicted")
	}

	// Now the same identity is admitted as new (dedup slot was released).
	if err := s.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("post-eviction re-persist should succeed, got %v", err)
	}
	if !hasRecord(s, "dup") {
		t.Fatalf("re-persisted identity must be stored after its slot was freed")
	}
}

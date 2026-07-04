package persistence_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// newFirstAttemptRecord builds a minimal Pending OutboxRecord for the
// first-attempt / replay-budget tests. CreatedAt is set so the record looks
// realistic; the first-attempt logic never reads it.
func newFirstAttemptRecord(t *testing.T) *persistence.OutboxRecord {
	t.Helper()
	rec, err := persistence.NewOutboxRecord(persistence.OutboxSpec{
		ID:         "rec-fa",
		RouteID:    "route-fa",
		EnvelopeID: "env-fa",
		BindingID:  "bind-fa",
		SessionID:  "sess-fa",
		Address:    "addr",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-fa"}),
		CreatedAt:  time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewOutboxRecord: %v", err)
	}
	return rec
}

// TestOutboxRecord_ClaimStampsFirstAttemptOnce proves the first-attempt clock is
// stamped exactly once — at the first claim — and never moved by a later
// release+reclaim or a stale-claim reclaim under a newer fencing token. This is
// the invariant the replay budget depends on: had Claim re-stamped on every
// claim, a chronically deferred record would keep resetting its budget and
// could never be poisoned.
func TestOutboxRecord_ClaimStampsFirstAttemptOnce(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("first claim stamps FirstAttemptedAt equal to ClaimedAt", func(t *testing.T) {
		rec := newFirstAttemptRecord(t)
		if !rec.FirstAttemptedAt().IsZero() {
			t.Fatalf("FirstAttemptedAt should be zero before any claim, got %v", rec.FirstAttemptedAt())
		}
		if err := rec.Claim(base, "owner-a", 1); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if !rec.FirstAttemptedAt().Equal(base) {
			t.Fatalf("FirstAttemptedAt = %v, want %v (== claim now)", rec.FirstAttemptedAt(), base)
		}
		if !rec.FirstAttemptedAt().Equal(rec.ClaimedAt()) {
			t.Fatalf("FirstAttemptedAt %v must equal ClaimedAt %v on the first claim", rec.FirstAttemptedAt(), rec.ClaimedAt())
		}
	})

	t.Run("release then reclaim keeps the original first attempt", func(t *testing.T) {
		rec := newFirstAttemptRecord(t)
		if err := rec.Claim(base, "owner-a", 1); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if err := rec.Release(base.Add(time.Minute)); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if err := rec.Claim(base.Add(2*time.Minute), "owner-a", 1); err != nil {
			t.Fatalf("re-Claim: %v", err)
		}
		if !rec.FirstAttemptedAt().Equal(base) {
			t.Fatalf("FirstAttemptedAt moved on reclaim: got %v, want %v", rec.FirstAttemptedAt(), base)
		}
		if !rec.ClaimedAt().Equal(base.Add(2 * time.Minute)) {
			t.Fatalf("ClaimedAt must advance to the reclaim instant: got %v, want %v", rec.ClaimedAt(), base.Add(2*time.Minute))
		}
		if rec.ReplayCount() != 2 {
			t.Fatalf("ReplayCount = %d, want 2 (two claims)", rec.ReplayCount())
		}
	})

	t.Run("stale reclaim by newer fencing token keeps the original first attempt", func(t *testing.T) {
		rec := newFirstAttemptRecord(t)
		if err := rec.Claim(base, "owner-a", 1); err != nil {
			t.Fatalf("Claim v1: %v", err)
		}
		// Reclaim at a higher fencing version WITHOUT releasing: the
		// stale-claim preemption path. FirstAttemptedAt must survive it.
		if err := rec.Claim(base.Add(2*time.Minute), "owner-b", 2); err != nil {
			t.Fatalf("Claim v2 (stale reclaim): %v", err)
		}
		if !rec.FirstAttemptedAt().Equal(base) {
			t.Fatalf("FirstAttemptedAt moved on stale reclaim: got %v, want %v", rec.FirstAttemptedAt(), base)
		}
	})
}

// TestOutboxRecord_SnapshotRoundTripsFirstAttempt proves PersistenceSnapshot and
// RehydrateFromSnapshot carry FirstAttemptedAt verbatim in both directions —
// a stamped instant survives and a zero (legacy / never-claimed) stays zero.
// Stores must never now-stamp a zero on a marshal/unmarshal round-trip.
func TestOutboxRecord_SnapshotRoundTripsFirstAttempt(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		claim bool
		want  time.Time
	}{
		{name: "claimed record round-trips the stamped first attempt", claim: true, want: base},
		{name: "never-claimed record round-trips a zero first attempt", claim: false, want: time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newFirstAttemptRecord(t)
			if tt.claim {
				if err := rec.Claim(base, "owner-a", 1); err != nil {
					t.Fatalf("Claim: %v", err)
				}
			}
			snap := rec.PersistenceSnapshot()
			if !snap.FirstAttemptedAt.Equal(tt.want) {
				t.Fatalf("snapshot FirstAttemptedAt = %v, want %v", snap.FirstAttemptedAt, tt.want)
			}
			rehydrated := persistence.RehydrateFromSnapshot(snap)
			if !rehydrated.FirstAttemptedAt().Equal(tt.want) {
				t.Fatalf("rehydrated FirstAttemptedAt = %v, want %v", rehydrated.FirstAttemptedAt(), tt.want)
			}
		})
	}
}

// TestOutboxRecord_ReleaseDoesNotMoveFirstAttempt pins plan Step 1.6: Release
// returns the record to Pending and clears ClaimedAt but MUST NOT touch
// firstAttemptedAt. A transient egress failure that releases the claim must not
// reset the replay budget.
func TestOutboxRecord_ReleaseDoesNotMoveFirstAttempt(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	rec := newFirstAttemptRecord(t)
	if err := rec.Claim(base, "owner-a", 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	first := rec.FirstAttemptedAt()
	if first.IsZero() {
		t.Fatal("precondition: first attempt should be stamped after the claim")
	}

	// Release with a LATER timestamp; it must not move firstAttemptedAt.
	if err := rec.Release(base.Add(time.Hour)); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !rec.FirstAttemptedAt().Equal(first) {
		t.Fatalf("Release moved FirstAttemptedAt: got %v, want %v", rec.FirstAttemptedAt(), first)
	}
	if !rec.ClaimedAt().IsZero() {
		t.Fatalf("Release should clear ClaimedAt, got %v", rec.ClaimedAt())
	}
}

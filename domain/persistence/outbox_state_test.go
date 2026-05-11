package persistence_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// validSpec returns a minimal OutboxSpec usable as a "good" baseline.
// Tests that need to seed lifecycle state (Status/ClaimVersion/...) use
// RehydrateFromSnapshot directly because those fields are not part of
// the construction contract.
func validSpec() persistence.OutboxSpec {
	return persistence.OutboxSpec{
		ID:         "rec-1",
		RouteID:    "route-1",
		EnvelopeID: "env-1",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   messaging.Envelope{ID: "env-1", Payload: []byte("p")},
		CreatedAt:  time.Unix(1_700_000_000, 0),
	}
}

// TestNewOutboxRecord_Construction validates that NewOutboxRecord
// enforces required identity fields and produces a Pending aggregate
// with zeroed lifecycle state.
func TestNewOutboxRecord_Construction(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*persistence.OutboxSpec)
		errIs  *shared.BridgeError // nil = expect no error
	}{
		{
			name:   "valid_spec_returns_pending",
			mutate: func(*persistence.OutboxSpec) {},
		},
		{
			name:   "missing_id_rejected",
			mutate: func(s *persistence.OutboxSpec) { s.ID = "" },
			errIs:  shared.ErrInvalidOutboxRecord,
		},
		{
			name: "missing_envelope_id_falls_back_to_envelope_id_field",
			mutate: func(s *persistence.OutboxSpec) {
				s.EnvelopeID = ""
				// s.Envelope.ID = "env-1" still set by validSpec
			},
		},
		{
			name: "missing_envelope_id_and_envelope_inner_id_rejected",
			mutate: func(s *persistence.OutboxSpec) {
				s.EnvelopeID = ""
				s.Envelope = messaging.Envelope{}
			},
			errIs: shared.ErrInvalidOutboxRecord,
		},
		{
			name: "missing_partition_keys_rejected",
			mutate: func(s *persistence.OutboxSpec) {
				s.SessionID = ""
				s.BindingID = ""
			},
			errIs: shared.ErrInvalidOutboxRecord,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			tc.mutate(&spec)
			rec, err := persistence.NewOutboxRecord(spec)
			if tc.errIs == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if rec.Status() != persistence.OutboxPending {
					t.Errorf("status = %q, want Pending", rec.Status())
				}
				if rec.ClaimVersion() != 0 {
					t.Errorf("claimVersion = %d, want 0", rec.ClaimVersion())
				}
				if rec.ReplayCount() != 0 {
					t.Errorf("replayCount = %d, want 0", rec.ReplayCount())
				}
				if !rec.ClaimedAt().IsZero() {
					t.Errorf("claimedAt should be zero, got %v", rec.ClaimedAt())
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tc.errIs) {
				t.Errorf("error = %v, want sentinel %v", err, tc.errIs)
			}
		})
	}
}

// TestOutboxRecord_IsClaimable enumerates the (status, claimVersion,
// givenTokenVersion) decision matrix.
func TestOutboxRecord_IsClaimable(t *testing.T) {
	tests := []struct {
		name          string
		status        persistence.OutboxStatus
		recVersion    uint64
		givenVersion  uint64
		wantClaimable bool
	}{
		{"pending_any_version", persistence.OutboxPending, 0, 1, true},
		{"pending_zero_version", persistence.OutboxPending, 0, 0, true},
		{"claimed_with_strictly_newer_token", persistence.OutboxClaimed, 1, 2, true},
		{"claimed_with_equal_token", persistence.OutboxClaimed, 2, 2, false},
		{"claimed_with_older_token", persistence.OutboxClaimed, 5, 3, false},
		{"completed_terminal", persistence.OutboxCompleted, 1, 9, false},
		{"expired_terminal", persistence.OutboxExpired, 1, 9, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := persistence.OutboxSnapshot{
				ID: "r", RouteID: "rt", EnvelopeID: "e", BindingID: "b", SessionID: "s",
				Status: tc.status, ClaimVersion: tc.recVersion,
			}
			rec := persistence.RehydrateFromSnapshot(snap)
			if got := rec.IsClaimable(tc.givenVersion); got != tc.wantClaimable {
				t.Errorf("IsClaimable(%d) = %v, want %v", tc.givenVersion, got, tc.wantClaimable)
			}
		})
	}
}

// TestOutboxRecord_Claim validates legal and illegal Claim transitions
// and the replay-count side-effect.
func TestOutboxRecord_Claim(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)

	t.Run("from_pending_succeeds_and_increments_replay", func(t *testing.T) {
		rec, _ := persistence.NewOutboxRecord(validSpec())
		if err := rec.Claim(now, "owner-A", 1); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if rec.Status() != persistence.OutboxClaimed {
			t.Errorf("status=%q want Claimed", rec.Status())
		}
		if rec.ClaimedBy() != "owner-A" {
			t.Errorf("claimedBy=%q", rec.ClaimedBy())
		}
		if rec.ClaimVersion() != 1 {
			t.Errorf("claimVersion=%d want 1", rec.ClaimVersion())
		}
		if rec.ReplayCount() != 1 {
			t.Errorf("replayCount=%d want 1", rec.ReplayCount())
		}
		if !rec.ClaimedAt().Equal(now) {
			t.Errorf("claimedAt=%v want %v", rec.ClaimedAt(), now)
		}
	})

	t.Run("stale_token_takeover_succeeds_and_bumps_replay", func(t *testing.T) {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: "r", RouteID: "rt", EnvelopeID: "e", BindingID: "b", SessionID: "s",
			Status: persistence.OutboxClaimed, ClaimVersion: 2, ReplayCount: 1,
			ClaimedBy: "old-owner",
		})
		if err := rec.Claim(now, "new-owner", 3); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if rec.ClaimedBy() != "new-owner" {
			t.Errorf("claimedBy=%q", rec.ClaimedBy())
		}
		if rec.ReplayCount() != 2 {
			t.Errorf("replayCount=%d want 2", rec.ReplayCount())
		}
	})

	t.Run("equal_token_rejected", func(t *testing.T) {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: "r", RouteID: "rt", EnvelopeID: "e", BindingID: "b", SessionID: "s",
			Status: persistence.OutboxClaimed, ClaimVersion: 5,
		})
		err := rec.Claim(now, "x", 5)
		if !errors.Is(err, shared.ErrOutboxNotClaimable) {
			t.Fatalf("err=%v want ErrOutboxNotClaimable", err)
		}
	})

	t.Run("from_completed_rejected", func(t *testing.T) {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: "r", RouteID: "rt", EnvelopeID: "e", BindingID: "b", SessionID: "s",
			Status: persistence.OutboxCompleted,
		})
		err := rec.Claim(now, "x", 99)
		if !errors.Is(err, shared.ErrOutboxNotClaimable) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("from_expired_rejected", func(t *testing.T) {
		rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: "r", RouteID: "rt", EnvelopeID: "e", BindingID: "b", SessionID: "s",
			Status: persistence.OutboxExpired,
		})
		err := rec.Claim(now, "x", 99)
		if !errors.Is(err, shared.ErrOutboxNotClaimable) {
			t.Fatalf("err=%v", err)
		}
	})
}

// TestOutboxRecord_Complete validates that Complete is only legal from
// the Claimed state.
func TestOutboxRecord_Complete(t *testing.T) {
	now := time.Unix(1_700_000_200, 0)

	t.Run("from_claimed_succeeds", func(t *testing.T) {
		rec, _ := persistence.NewOutboxRecord(validSpec())
		_ = rec.Claim(now, "o", 1)
		if err := rec.Complete(now); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if rec.Status() != persistence.OutboxCompleted {
			t.Errorf("status=%q", rec.Status())
		}
		if !rec.CompletedAt().Equal(now) {
			t.Errorf("completedAt=%v", rec.CompletedAt())
		}
	})

	for _, st := range []persistence.OutboxStatus{
		persistence.OutboxPending, persistence.OutboxCompleted, persistence.OutboxExpired,
	} {
		st := st
		t.Run("from_"+string(st)+"_rejected", func(t *testing.T) {
			rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
				ID: "r", RouteID: "rt", EnvelopeID: "e", BindingID: "b", SessionID: "s",
				Status: st,
			})
			err := rec.Complete(now)
			if !errors.Is(err, shared.ErrOutboxNotInClaimedState) {
				t.Fatalf("err=%v want ErrOutboxNotInClaimedState", err)
			}
		})
	}
}

// TestOutboxRecord_Expire validates legal transitions from Pending and
// Claimed plus rejection from terminal states.
func TestOutboxRecord_Expire(t *testing.T) {
	now := time.Unix(1_700_000_300, 0)

	for _, st := range []persistence.OutboxStatus{persistence.OutboxPending, persistence.OutboxClaimed} {
		st := st
		t.Run("from_"+string(st)+"_succeeds", func(t *testing.T) {
			rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
				ID: "r", RouteID: "rt", EnvelopeID: "e", BindingID: "b", SessionID: "s",
				Status: st,
			})
			if err := rec.Expire(now); err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if rec.Status() != persistence.OutboxExpired {
				t.Errorf("status=%q", rec.Status())
			}
		})
	}

	for _, st := range []persistence.OutboxStatus{persistence.OutboxCompleted, persistence.OutboxExpired} {
		st := st
		t.Run("from_"+string(st)+"_rejected", func(t *testing.T) {
			rec := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
				ID: "r", RouteID: "rt", EnvelopeID: "e", BindingID: "b", SessionID: "s",
				Status: st,
			})
			err := rec.Expire(now)
			if !errors.Is(err, shared.ErrOutboxAlreadyTerminal) {
				t.Fatalf("err=%v want ErrOutboxAlreadyTerminal", err)
			}
		})
	}
}

// TestOutboxRecord_SnapshotRoundTrip validates that Snapshot()+Rehydrate()
// is loss-free for every lifecycle field.
func TestOutboxRecord_SnapshotRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_400, 0)
	original := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:           "rec-rt",
		RouteID:      "route-rt",
		EnvelopeID:   "env-rt",
		BindingID:    "bind-rt",
		SessionID:    "sess-rt",
		Address:      "tcp://x",
		Envelope:     messaging.Envelope{ID: "env-rt", Payload: []byte("p")},
		Status:       persistence.OutboxClaimed,
		ClaimedBy:    "owner-x",
		ClaimedAt:    now,
		ClaimVersion: 7,
		ReplayCount:  3,
		CompletedAt:  time.Time{},
		CreatedAt:    now.Add(-time.Hour),
		ExpiresAt:    now.Add(time.Hour),
	})

	snap := original.PersistenceSnapshot()
	clone := persistence.RehydrateFromSnapshot(snap)

	if clone.Status() != original.Status() ||
		clone.ClaimedBy() != original.ClaimedBy() ||
		!clone.ClaimedAt().Equal(original.ClaimedAt()) ||
		clone.ClaimVersion() != original.ClaimVersion() ||
		clone.ReplayCount() != original.ReplayCount() ||
		!clone.CompletedAt().Equal(original.CompletedAt()) ||
		clone.ID != original.ID ||
		clone.RouteID != original.RouteID ||
		clone.EnvelopeID != original.EnvelopeID ||
		clone.Envelope.ID != original.Envelope.ID {
		t.Fatalf("snapshot round-trip lost state\norig=%+v\nclone=%+v", original.PersistenceSnapshot(), snap)
	}
}

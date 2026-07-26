package persistence_test

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// mustCommitted returns a Committed rollout (all acked, then committed at v3).
func mustCommitted(t *testing.T) persistence.Rollout {
	t.Helper()
	c, err := mustStaged(t).WithCommit(tok(3))
	if err != nil {
		t.Fatalf("WithCommit: %v", err)
	}
	return c
}

// mustAborted returns an Aborted rollout (aborted from Proposed at v2).
func mustAborted(t *testing.T) persistence.Rollout {
	t.Helper()
	a, err := mustPropose(t).WithAbort(tok(2), "deadline")
	if err != nil {
		t.Fatalf("WithAbort: %v", err)
	}
	return a
}

// mustStagingWithNack returns a Staging rollout with one ack and one nack, to
// prove both maps round-trip through a snapshot.
func mustStagingWithNack(t *testing.T) persistence.Rollout {
	t.Helper()
	r, err := mustPropose(t).WithAck("node-a", "build:a", time.Unix(2, 0))
	if err != nil {
		t.Fatalf("WithAck: %v", err)
	}
	r, err = r.WithNack("node-b", "plugin missing")
	if err != nil {
		t.Fatalf("WithNack: %v", err)
	}
	return r
}

func assertSameRollout(t *testing.T, got, want persistence.Rollout) {
	t.Helper()
	if got.Generation() != want.Generation() {
		t.Fatalf("generation = %d, want %d", got.Generation(), want.Generation())
	}
	if got.State() != want.State() {
		t.Fatalf("state = %q, want %q", got.State(), want.State())
	}
	if got.ConfigDigest() != want.ConfigDigest() || got.ConfigVersion() != want.ConfigVersion() {
		t.Fatalf("digest/version = %q/%d, want %q/%d",
			got.ConfigDigest(), got.ConfigVersion(), want.ConfigDigest(), want.ConfigVersion())
	}
	if got.Reason() != want.Reason() {
		t.Fatalf("reason = %q, want %q", got.Reason(), want.Reason())
	}
	if got.CoordinatorVersion() != want.CoordinatorVersion() {
		t.Fatalf("coordinator version = %d, want %d", got.CoordinatorVersion(), want.CoordinatorVersion())
	}
	if !got.Deadline().Equal(want.Deadline()) {
		t.Fatalf("deadline = %v, want %v", got.Deadline(), want.Deadline())
	}
	if !slices.Equal(got.MembershipEpoch(), want.MembershipEpoch()) {
		t.Fatalf("epoch = %v, want %v", got.MembershipEpoch(), want.MembershipEpoch())
	}
	if !maps.Equal(got.Nacks(), want.Nacks()) {
		t.Fatalf("nacks = %v, want %v", got.Nacks(), want.Nacks())
	}
	ga, wa := got.Acks(), want.Acks()
	if len(ga) != len(wa) {
		t.Fatalf("acks = %v, want %v", ga, wa)
	}
	for k, wv := range wa {
		gv, ok := ga[k]
		if !ok || gv.MemberID != wv.MemberID || gv.BuildDigest != wv.BuildDigest || !gv.At.Equal(wv.At) {
			t.Fatalf("ack[%s] = %+v, want %+v", k, gv, wv)
		}
	}
}

// TestRollout_SnapshotRehydrateRoundTrip proves a rollout survives a full
// Snapshot -> RehydrateRollout round trip byte-for-byte for every lifecycle
// state -- the property a durable store adapter relies on to persist and reload
// the aggregate without a hand-rolled parallel state machine.
func TestRollout_SnapshotRehydrateRoundTrip(t *testing.T) {
	cases := map[string]persistence.Rollout{
		"proposed":           mustPropose(t),
		"staging_all_acked":  mustStaged(t),
		"staging_ack_+ nack": mustStagingWithNack(t),
		"committed":          mustCommitted(t),
		"aborted":            mustAborted(t),
	}
	for name, orig := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := persistence.RehydrateRollout(orig.Snapshot())
			if err != nil {
				t.Fatalf("RehydrateRollout: %v", err)
			}
			assertSameRollout(t, got, orig)
		})
	}
}

// TestRehydrateRollout_ProducesLiveAggregate proves the reconstituted value is a
// fully-functional aggregate, not a frozen shell: a rehydrated Staging rollout
// still commits, and a rehydrated terminal rollout still enforces
// terminal-immutability AND the fencing high-water mark (invariants I3/I4). This
// is the property the store's compare-and-set retry loop depends on -- it
// re-reads, rehydrates, and re-applies a transition every attempt.
func TestRehydrateRollout_ProducesLiveAggregate(t *testing.T) {
	staged, err := persistence.RehydrateRollout(mustStaged(t).Snapshot())
	if err != nil {
		t.Fatalf("rehydrate staged: %v", err)
	}
	c, cerr := staged.WithCommit(tok(5))
	if cerr != nil {
		t.Fatalf("rehydrated staging must commit: %v", cerr)
	}
	if c.State() != persistence.RolloutCommitted || c.CoordinatorVersion() != 5 {
		t.Fatalf("committed state/version = %q/%d", c.State(), c.CoordinatorVersion())
	}

	committed, err := persistence.RehydrateRollout(mustCommitted(t).Snapshot())
	if err != nil {
		t.Fatalf("rehydrate committed: %v", err)
	}
	if _, e := committed.WithAbort(tok(9), "x"); !errors.Is(e, shared.ErrRolloutTerminal) {
		t.Fatalf("rehydrated committed abort: err = %v, want ErrRolloutTerminal", e)
	}
	// coordVersion (3) must have been restored, so a lower-versioned decision is
	// rejected as a deposed coordinator's stale token.
	if _, e := committed.WithCommit(tok(1)); !errors.Is(e, shared.ErrStaleFencingToken) {
		t.Fatalf("rehydrated committed stale re-commit: err = %v, want ErrStaleFencingToken", e)
	}
}

// TestRehydrateRollout_RejectsCorruptSnapshot proves the reconstitution factory
// fails closed on a malformed/corrupt persisted snapshot rather than producing a
// broken aggregate a store could then act on.
func TestRehydrateRollout_RejectsCorruptSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*persistence.RolloutSnapshot)
	}{
		{"zero_generation", func(s *persistence.RolloutSnapshot) { s.Generation = 0 }},
		{"unknown_state", func(s *persistence.RolloutSnapshot) { s.State = "bogus" }},
		{"empty_epoch", func(s *persistence.RolloutSnapshot) { s.MembershipEpoch = nil }},
		{"empty_member", func(s *persistence.RolloutSnapshot) { s.MembershipEpoch = []string{"node-a", ""} }},
		{"duplicate_member", func(s *persistence.RolloutSnapshot) { s.MembershipEpoch = []string{"node-a", "node-a"} }},
		{"ack_from_stranger", func(s *persistence.RolloutSnapshot) {
			s.Acks = map[string]persistence.RolloutAck{"stranger": {MemberID: "stranger", BuildDigest: "b"}}
		}},
		{"nack_from_stranger", func(s *persistence.RolloutSnapshot) {
			s.Nacks = map[string]string{"stranger": "x"}
		}},
		{"terminal_without_coord_version", func(s *persistence.RolloutSnapshot) {
			s.State = persistence.RolloutCommitted
			s.CoordinatorVersion = 0
		}},
		{"non_terminal_with_coord_version", func(s *persistence.RolloutSnapshot) {
			s.State = persistence.RolloutStaging
			s.CoordinatorVersion = 4
		}},
		{"proposed_with_acks", func(s *persistence.RolloutSnapshot) {
			s.State = persistence.RolloutProposed // base has acks: proposed can carry none
		}},
		{"staging_without_acks", func(s *persistence.RolloutSnapshot) {
			s.Acks = map[string]persistence.RolloutAck{} // staging needs >= 1 ack
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := mustStaged(t).Snapshot()
			tc.mutate(&snap)
			if _, err := persistence.RehydrateRollout(snap); !errors.Is(err, shared.ErrInvalidRolloutProposal) {
				t.Fatalf("corrupt snapshot: err = %v, want ErrInvalidRolloutProposal", err)
			}
		})
	}
}

// TestRehydrateRollout_DefensiveCopy proves neither Snapshot nor RehydrateRollout
// aliases caller-mutable maps/slices into the immutable aggregate.
func TestRehydrateRollout_DefensiveCopy(t *testing.T) {
	orig := mustStagingWithNack(t)
	snap := orig.Snapshot()

	// Mutating the snapshot after taking it must not affect the source rollout.
	snap.MembershipEpoch[0] = "hacked"
	snap.Acks["ghost"] = persistence.RolloutAck{MemberID: "ghost"}
	snap.Nacks["ghost"] = "x"
	if slices.Contains(orig.MembershipEpoch(), "hacked") {
		t.Fatal("Snapshot aliased the aggregate's epoch slice")
	}

	// Rehydrating from the (now-mutated) snapshot and mutating it again must not
	// affect the rehydrated aggregate either.
	snap2 := mustStaged(t).Snapshot()
	r, err := persistence.RehydrateRollout(snap2)
	if err != nil {
		t.Fatalf("RehydrateRollout: %v", err)
	}
	snap2.MembershipEpoch[0] = "hacked"
	snap2.Acks["ghost"] = persistence.RolloutAck{MemberID: "ghost"}
	if slices.Contains(r.MembershipEpoch(), "hacked") {
		t.Fatal("RehydrateRollout aliased the snapshot's epoch slice")
	}
	if _, ok := r.Acks()["ghost"]; ok {
		t.Fatal("RehydrateRollout aliased the snapshot's Acks map")
	}
}

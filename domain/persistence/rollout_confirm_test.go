package persistence_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// The confirm window (design §8.1) layers a NETCONF/NSO "provisional apply with
// deadman timer" on top of the base barrier. These tests pin the aggregate's
// half: a windowed Commit is NON-terminal, members Converge, a fenced Confirm
// (all converged) or Revert makes it terminal, and the base protocol
// (confirm_window == 0) is unchanged.

var confirmDeadline = time.Unix(1_700_000_000, 0).Add(90 * time.Second)

// mustProvisionalCommit returns a windowed (provisional) committed rollout: a
// proposal carrying a confirm window, all three members acked, then
// WithProvisionalCommit stamps a confirm deadline. The window on the proposal is
// what a real provisional commit always carries (the store only calls
// WithProvisionalCommit when ConfirmWindow > 0).
func mustProvisionalCommit(t *testing.T) persistence.Rollout {
	t.Helper()
	p := validProposal()
	p.ConfirmWindow = 90 * time.Second
	r, err := persistence.NewRollout(1, p, epochDeadline)
	if err != nil {
		t.Fatalf("NewRollout: %v", err)
	}
	for _, m := range []string{"node-a", "node-b", "node-c"} {
		r, err = r.WithAck(m, "build:"+m, time.Unix(1, 0))
		if err != nil {
			t.Fatalf("WithAck(%s): %v", m, err)
		}
	}
	c, err := r.WithProvisionalCommit(tok(3), confirmDeadline)
	if err != nil {
		t.Fatalf("WithProvisionalCommit: %v", err)
	}
	return c
}

// mustConverged returns a provisional-committed rollout with every member
// converged (confirm-ready).
func mustConverged(t *testing.T) persistence.Rollout {
	t.Helper()
	r := mustProvisionalCommit(t)
	for _, m := range []string{"node-a", "node-b", "node-c"} {
		var err *shared.BridgeError
		r, err = r.WithConverged(m, time.Unix(2, 0))
		if err != nil {
			t.Fatalf("WithConverged(%s): %v", m, err)
		}
	}
	return r
}

// ─────────────────────────────────────────────────────────────────────────
// Terminality: base Commit terminal, windowed Commit NOT terminal
// ─────────────────────────────────────────────────────────────────────────

func TestRollout_ProvisionalCommit_IsNotTerminal(t *testing.T) {
	c := mustProvisionalCommit(t)
	if c.State() != persistence.RolloutCommitted {
		t.Fatalf("state = %q, want committed", c.State())
	}
	if c.IsTerminal() {
		t.Fatal("a windowed (provisional) committed rollout must NOT be terminal: it awaits confirm/revert")
	}
	if !c.ConfirmDeadline().Equal(confirmDeadline) {
		t.Fatalf("confirm deadline = %v, want %v", c.ConfirmDeadline(), confirmDeadline)
	}
	if c.CoordinatorVersion() != 3 {
		t.Fatalf("coordinator version = %d, want 3 (fence stamped at commit)", c.CoordinatorVersion())
	}
}

func TestRollout_BaseCommit_StaysTerminal(t *testing.T) {
	// confirm_window == 0: WithCommit is unchanged, Committed is terminal.
	c, err := mustStaged(t).WithCommit(tok(1))
	if err != nil {
		t.Fatalf("WithCommit: %v", err)
	}
	if !c.IsTerminal() {
		t.Fatal("a base-protocol committed rollout must be terminal (unchanged behavior)")
	}
	if !c.ConfirmDeadline().IsZero() {
		t.Fatal("a base-protocol commit must not stamp a confirm deadline")
	}
}

func TestRolloutState_IsTerminal_InherentStates(t *testing.T) {
	// RolloutState.IsTerminal answers "inherently terminal regardless of window".
	// Committed is NOT inherent (it depends on the window) -- use Rollout.IsTerminal.
	cases := map[persistence.RolloutState]bool{
		persistence.RolloutProposed:  false,
		persistence.RolloutStaging:   false,
		persistence.RolloutCommitted: false,
		persistence.RolloutAborted:   true,
		persistence.RolloutConfirmed: true,
		persistence.RolloutReverted:  true,
	}
	for s, want := range cases {
		if s.IsTerminal() != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, s.IsTerminal(), want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// WithConverged (I6: at-most-once, member-in-epoch, only while windowed-committed)
// ─────────────────────────────────────────────────────────────────────────

func TestRollout_Converged_Records(t *testing.T) {
	c := mustProvisionalCommit(t)
	r, err := c.WithConverged("node-a", time.Unix(2, 0))
	if err != nil {
		t.Fatalf("WithConverged: %v", err)
	}
	got, ok := r.Converged()["node-a"]
	if !ok || got.MemberID != "node-a" {
		t.Fatalf("convergence not recorded: %+v", r.Converged())
	}
	if r.State() != persistence.RolloutCommitted {
		t.Fatalf("convergence must not change state, got %q", r.State())
	}
	// immutable: receiver unchanged.
	if len(c.Converged()) != 0 {
		t.Fatal("WithConverged mutated the receiver")
	}
}

func TestRollout_Converged_Rejections(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) persistence.Rollout
		mem   string
		want  *shared.BridgeError
	}{
		{"member_not_in_epoch", mustProvisionalCommit, "stranger", shared.ErrRolloutAckRejected},
		{"already_converged", func(t *testing.T) persistence.Rollout {
			r, _ := mustProvisionalCommit(t).WithConverged("node-a", time.Unix(2, 0))
			return r
		}, "node-a", shared.ErrRolloutAckRejected},
		{"not_yet_committed", mustStaged, "node-a", shared.ErrRolloutNotConfirmable},
		{"base_committed_no_window", func(t *testing.T) persistence.Rollout {
			c, _ := mustStaged(t).WithCommit(tok(1))
			return c
		}, "node-a", shared.ErrRolloutTerminal},
		{"already_confirmed", mustConfirmed, "node-a", shared.ErrRolloutTerminal},
		{"already_reverted", mustReverted, "node-a", shared.ErrRolloutTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.setup(t).WithConverged(tc.mem, time.Unix(2, 0))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// WithConfirm (I7: all converged; I3: fencing; terminal → Confirmed)
// ─────────────────────────────────────────────────────────────────────────

func mustConfirmed(t *testing.T) persistence.Rollout {
	t.Helper()
	c, err := mustConverged(t).WithConfirm(tok(3))
	if err != nil {
		t.Fatalf("WithConfirm: %v", err)
	}
	return c
}

func TestRollout_Confirm_RequiresAllConverged(t *testing.T) {
	c := mustProvisionalCommit(t)
	c, _ = c.WithConverged("node-a", time.Unix(2, 0)) // only 1 of 3
	if c.CanConfirm() {
		t.Fatal("CanConfirm() true with incomplete convergence")
	}
	_, err := c.WithConfirm(tok(3))
	if !errors.Is(err, shared.ErrRolloutNotConfirmable) {
		t.Fatalf("err = %v, want ErrRolloutNotConfirmable", err)
	}
}

func TestRollout_Confirm_Succeeds(t *testing.T) {
	c := mustConverged(t)
	if !c.CanConfirm() {
		t.Fatal("CanConfirm() false after all converged")
	}
	confirmed, err := c.WithConfirm(tok(3))
	if err != nil {
		t.Fatalf("WithConfirm: %v", err)
	}
	if confirmed.State() != persistence.RolloutConfirmed {
		t.Fatalf("state = %q, want confirmed", confirmed.State())
	}
	if !confirmed.IsTerminal() {
		t.Fatal("a confirmed rollout must be terminal")
	}
}

func TestRollout_Confirm_Idempotent(t *testing.T) {
	confirmed := mustConfirmed(t)
	again, err := confirmed.WithConfirm(tok(5)) // same-or-newer token
	if err != nil {
		t.Fatalf("idempotent re-confirm err = %v", err)
	}
	if again.State() != persistence.RolloutConfirmed {
		t.Fatalf("state = %q, want confirmed", again.State())
	}
}

func TestRollout_Confirm_StaleTokenRejected(t *testing.T) {
	// coordVersion is 3 (stamped at the provisional commit); an older coordinator
	// cannot confirm.
	c := mustConverged(t)
	_, err := c.WithConfirm(tok(2))
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("err = %v, want ErrStaleFencingToken", err)
	}
}

func TestRollout_Confirm_OnBaseCommittedRejected(t *testing.T) {
	// A base-protocol committed rollout has no window to confirm.
	c, _ := mustStaged(t).WithCommit(tok(1))
	_, err := c.WithConfirm(tok(1))
	if !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("confirm-of-base-committed err = %v, want ErrRolloutTerminal", err)
	}
}

func TestRollout_Confirm_OnRevertedRejected(t *testing.T) {
	_, err := mustReverted(t).WithConfirm(tok(9))
	if !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("confirm-of-reverted err = %v, want ErrRolloutTerminal", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// WithRevert (deadman outcome; I3: fencing; terminal → Reverted)
// ─────────────────────────────────────────────────────────────────────────

func mustReverted(t *testing.T) persistence.Rollout {
	t.Helper()
	r, err := mustProvisionalCommit(t).WithRevert(tok(3), "confirm window expired")
	if err != nil {
		t.Fatalf("WithRevert: %v", err)
	}
	return r
}

func TestRollout_Revert_Succeeds(t *testing.T) {
	r := mustReverted(t)
	if r.State() != persistence.RolloutReverted {
		t.Fatalf("state = %q, want reverted", r.State())
	}
	if !r.IsTerminal() {
		t.Fatal("a reverted rollout must be terminal")
	}
	if r.Reason() != "confirm window expired" {
		t.Fatalf("reason = %q", r.Reason())
	}
}

func TestRollout_Revert_Idempotent(t *testing.T) {
	r := mustReverted(t)
	again, err := r.WithRevert(tok(9), "other")
	if err != nil {
		t.Fatalf("idempotent re-revert err = %v", err)
	}
	if again.State() != persistence.RolloutReverted {
		t.Fatalf("state = %q, want reverted", again.State())
	}
}

func TestRollout_Revert_StaleTokenRejected(t *testing.T) {
	_, err := mustConverged(t).WithRevert(tok(2), "x")
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("err = %v, want ErrStaleFencingToken", err)
	}
}

func TestRollout_Revert_OnConfirmedRejected(t *testing.T) {
	_, err := mustConfirmed(t).WithRevert(tok(9), "x")
	if !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("revert-of-confirmed err = %v, want ErrRolloutTerminal", err)
	}
}

func TestRollout_Revert_OnBaseCommittedRejected(t *testing.T) {
	c, _ := mustStaged(t).WithCommit(tok(1))
	_, err := c.WithRevert(tok(1), "x")
	if !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("revert-of-base-committed err = %v, want ErrRolloutTerminal", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// ConfirmWindow frozen from the proposal
// ─────────────────────────────────────────────────────────────────────────

func TestNewRollout_FreezesConfirmWindow(t *testing.T) {
	p := validProposal()
	p.ConfirmWindow = 90 * time.Second
	r, err := persistence.NewRollout(1, p, epochDeadline)
	if err != nil {
		t.Fatalf("NewRollout: %v", err)
	}
	if r.ConfirmWindow() != 90*time.Second {
		t.Fatalf("confirm window = %v, want 90s", r.ConfirmWindow())
	}
	// Before commit there is no confirm deadline.
	if !r.ConfirmDeadline().IsZero() {
		t.Fatal("a proposed rollout must not carry a confirm deadline")
	}
}

// TestRollout_ConfirmWindow_SnapshotRoundTrip proves the confirm-window fields
// (window, deadline, converged set) survive Snapshot -> RehydrateRollout for the
// windowed states a store must persist and reload.
func TestRollout_ConfirmWindow_SnapshotRoundTrip(t *testing.T) {
	cases := map[string]persistence.Rollout{
		"provisional_committed": mustProvisionalCommit(t),
		"converged":             mustConverged(t),
		"confirmed":             mustConfirmed(t),
		"reverted":              mustReverted(t),
	}
	for name, orig := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := persistence.RehydrateRollout(orig.Snapshot())
			if err != nil {
				t.Fatalf("RehydrateRollout: %v", err)
			}
			if got.State() != orig.State() {
				t.Fatalf("state = %q, want %q", got.State(), orig.State())
			}
			if got.ConfirmWindow() != orig.ConfirmWindow() {
				t.Fatalf("confirm window = %v, want %v", got.ConfirmWindow(), orig.ConfirmWindow())
			}
			if !got.ConfirmDeadline().Equal(orig.ConfirmDeadline()) {
				t.Fatalf("confirm deadline = %v, want %v", got.ConfirmDeadline(), orig.ConfirmDeadline())
			}
			if len(got.Converged()) != len(orig.Converged()) {
				t.Fatalf("converged = %v, want %v", got.Converged(), orig.Converged())
			}
			for m := range orig.Converged() {
				if _, ok := got.Converged()[m]; !ok {
					t.Fatalf("converged missing %q after round trip", m)
				}
			}
		})
	}
}

func TestRehydrateRollout_RejectsCorruptConfirmWindow(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*persistence.RolloutSnapshot)
	}{
		{"converged_before_commit", func(s *persistence.RolloutSnapshot) {
			s.Converged = map[string]persistence.RolloutConverged{"node-a": {MemberID: "node-a"}}
		}},
		{"converged_from_stranger", func(s *persistence.RolloutSnapshot) {
			s.State = persistence.RolloutCommitted
			s.CoordinatorVersion = 3
			s.ConfirmDeadline = confirmDeadline
			s.Converged = map[string]persistence.RolloutConverged{"stranger": {MemberID: "stranger"}}
		}},
		{"deadline_before_commit", func(s *persistence.RolloutSnapshot) {
			s.ConfirmDeadline = confirmDeadline // still Staging
		}},
		{"confirmed_missing_convergence", func(s *persistence.RolloutSnapshot) {
			s.State = persistence.RolloutConfirmed
			s.CoordinatorVersion = 3
			s.ConfirmDeadline = confirmDeadline
			s.Converged = map[string]persistence.RolloutConverged{"node-a": {MemberID: "node-a"}} // only 1 of 3
		}},
		{"windowed_committed_missing_deadline", func(s *persistence.RolloutSnapshot) {
			// A windowed commit whose confirm deadline was dropped/corrupted would
			// otherwise rehydrate as a TERMINAL final commit, silently skipping the
			// confirm barrier. Must fail closed (adversarial-review finding).
			s.State = persistence.RolloutCommitted
			s.CoordinatorVersion = 3
			s.ConfirmWindow = 90 * time.Second
			s.ConfirmDeadline = time.Time{} // dropped
		}},
		{"base_committed_with_stray_deadline", func(s *persistence.RolloutSnapshot) {
			// The reverse: a base (final) commit must not carry a confirm deadline, or
			// it would rehydrate as a phantom provisional and revert at the stray time.
			s.State = persistence.RolloutCommitted
			s.CoordinatorVersion = 3
			s.ConfirmWindow = 0
			s.ConfirmDeadline = confirmDeadline
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := mustStaged(t).Snapshot() // epoch a/b/c, Staging
			tc.mutate(&snap)
			if _, err := persistence.RehydrateRollout(snap); !errors.Is(err, shared.ErrInvalidRolloutProposal) {
				t.Fatalf("corrupt confirm-window snapshot: err = %v, want ErrInvalidRolloutProposal", err)
			}
		})
	}
}

// The vote phase is over once committed: ack/nack a windowed-committed rollout is
// rejected exactly like a terminal one (voting closed at commit).
func TestRollout_Ack_OnProvisionalCommittedRejected(t *testing.T) {
	c := mustProvisionalCommit(t)
	if _, err := c.WithAck("node-a", "b", time.Unix(3, 0)); !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("ack-after-provisional-commit err = %v, want ErrRolloutTerminal", err)
	}
	if _, err := c.WithNack("node-a", "late"); !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("nack-after-provisional-commit err = %v, want ErrRolloutTerminal", err)
	}
}

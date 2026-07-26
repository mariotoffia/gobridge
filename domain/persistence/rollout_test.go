package persistence_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// validProposal returns a baseline proposal each test mutates.
func validProposal() persistence.RolloutProposal {
	return persistence.RolloutProposal{
		ProposerID:    "node-a",
		ConfigDigest:  "sha256:deadbeef",
		ConfigVersion: 7,
		Members:       []string{"node-c", "node-a", "node-b"}, // unsorted on purpose
		TTL:           5 * time.Minute,
	}
}

var epochDeadline = time.Unix(1_700_000_000, 0).Add(5 * time.Minute)

func mustPropose(t *testing.T) persistence.Rollout {
	t.Helper()
	r, err := persistence.NewRollout(1, validProposal(), epochDeadline)
	if err != nil {
		t.Fatalf("NewRollout: %v", err)
	}
	return r
}

// mustStaged returns a Staging rollout with all three members acked (commit-ready).
func mustStaged(t *testing.T) persistence.Rollout {
	t.Helper()
	r := mustPropose(t)
	for _, m := range []string{"node-a", "node-b", "node-c"} {
		var err *shared.BridgeError
		r, err = r.WithAck(m, "build:"+m, time.Unix(1, 0))
		if err != nil {
			t.Fatalf("WithAck(%s): %v", m, err)
		}
	}
	return r
}

func tok(v uint64) persistence.LeaseToken { return persistence.LeaseToken{Version: v, Owner: "coord"} }

// ─────────────────────────────────────────────────────────────────────────
// NewRollout + proposal validation
// ─────────────────────────────────────────────────────────────────────────

func TestNewRollout_CreatesProposed(t *testing.T) {
	r := mustPropose(t)
	if r.State() != persistence.RolloutProposed {
		t.Fatalf("state = %q, want proposed", r.State())
	}
	if r.Generation() != 1 {
		t.Fatalf("generation = %d, want 1", r.Generation())
	}
	if r.ConfigDigest() != "sha256:deadbeef" || r.ConfigVersion() != 7 {
		t.Fatalf("digest/version = %q/%d", r.ConfigDigest(), r.ConfigVersion())
	}
	if !r.Deadline().Equal(epochDeadline) {
		t.Fatalf("deadline = %v, want %v", r.Deadline(), epochDeadline)
	}
	// membership must be SORTED (deterministic epoch) and complete.
	want := []string{"node-a", "node-b", "node-c"}
	got := r.MembershipEpoch()
	if len(got) != len(want) {
		t.Fatalf("epoch = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("epoch = %v, want sorted %v", got, want)
		}
	}
	if len(r.Acks()) != 0 || len(r.Nacks()) != 0 {
		t.Fatalf("fresh rollout must have no acks/nacks")
	}
	if r.CoordinatorVersion() != 0 {
		t.Fatalf("fresh rollout coordinator version = %d, want 0", r.CoordinatorVersion())
	}
}

func TestNewRollout_RejectsInvalidProposal(t *testing.T) {
	cases := []struct {
		name   string
		gen    uint64
		mutate func(*persistence.RolloutProposal)
	}{
		{"zero_generation", 0, func(p *persistence.RolloutProposal) {}},
		{"empty_proposer", 1, func(p *persistence.RolloutProposal) { p.ProposerID = "" }},
		{"empty_digest", 1, func(p *persistence.RolloutProposal) { p.ConfigDigest = "" }},
		{"no_members", 1, func(p *persistence.RolloutProposal) { p.Members = nil }},
		{"empty_member_id", 1, func(p *persistence.RolloutProposal) { p.Members = []string{"node-a", ""} }},
		{"duplicate_members", 1, func(p *persistence.RolloutProposal) { p.Members = []string{"node-a", "node-a"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProposal()
			tc.mutate(&p)
			_, err := persistence.NewRollout(tc.gen, p, epochDeadline)
			if !errors.Is(err, shared.ErrInvalidRolloutProposal) {
				t.Fatalf("err = %v, want ErrInvalidRolloutProposal", err)
			}
		})
	}
}

func TestNewRollout_DefensiveCopy(t *testing.T) {
	p := validProposal()
	r, err := persistence.NewRollout(1, p, epochDeadline)
	if err != nil {
		t.Fatalf("NewRollout: %v", err)
	}
	p.Members[0] = "hacked" // mutate caller slice AFTER construction
	for _, m := range r.MembershipEpoch() {
		if m == "hacked" {
			t.Fatal("rollout aliased the caller's Members slice")
		}
	}
	// Mutating a returned accessor map must not affect the rollout.
	r.Acks()["ghost"] = persistence.RolloutAck{MemberID: "ghost"}
	if len(r.Acks()) != 0 {
		t.Fatal("Acks() returned a live map reference")
	}
	r.Nacks()["ghost"] = "x"
	if len(r.Nacks()) != 0 {
		t.Fatal("Nacks() returned a live map reference")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// RolloutState
// ─────────────────────────────────────────────────────────────────────────

// RolloutState.IsTerminal (inherent-terminality, window-aware Rollout.IsTerminal,
// and the two new states) is covered by TestRolloutState_IsTerminal_InherentStates
// in rollout_confirm_test.go.

// ─────────────────────────────────────────────────────────────────────────
// WithAck (I5: at-most-once, member-in-epoch, not-terminal)
// ─────────────────────────────────────────────────────────────────────────

func TestRollout_Ack_FirstAckMovesToStaging(t *testing.T) {
	r := mustPropose(t)
	r2, err := r.WithAck("node-a", "build:1", time.Unix(1, 0))
	if err != nil {
		t.Fatalf("WithAck: %v", err)
	}
	if r2.State() != persistence.RolloutStaging {
		t.Fatalf("state = %q, want staging", r2.State())
	}
	ack, ok := r2.Acks()["node-a"]
	if !ok || ack.BuildDigest != "build:1" {
		t.Fatalf("ack not recorded: %+v", r2.Acks())
	}
	// original rollout is unchanged (immutable transition).
	if r.State() != persistence.RolloutProposed || len(r.Acks()) != 0 {
		t.Fatal("WithAck mutated the receiver")
	}
}

func TestRollout_Ack_Rejections(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) persistence.Rollout
		mem   string
	}{
		{"member_not_in_epoch", mustPropose, "stranger"},
		{"already_acked", func(t *testing.T) persistence.Rollout {
			r := mustPropose(t)
			r, _ = r.WithAck("node-a", "b", time.Unix(1, 0))
			return r
		}, "node-a"},
		{"already_nacked", func(t *testing.T) persistence.Rollout {
			r := mustPropose(t)
			r, _ = r.WithNack("node-a", "bad")
			return r
		}, "node-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.setup(t)
			_, err := r.WithAck(tc.mem, "b", time.Unix(1, 0))
			if !errors.Is(err, shared.ErrRolloutAckRejected) {
				t.Fatalf("err = %v, want ErrRolloutAckRejected", err)
			}
		})
	}
}

func TestRollout_Ack_OnTerminalRejected(t *testing.T) {
	r := mustStaged(t)
	committed, err := r.WithCommit(tok(1))
	if err != nil {
		t.Fatalf("WithCommit: %v", err)
	}
	_, err = committed.WithAck("node-a", "b", time.Unix(1, 0))
	if !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("ack-after-commit err = %v, want ErrRolloutTerminal", err)
	}
}

func TestRollout_Ack_EmptyBuildDigestRejected(t *testing.T) {
	// An ack with no build digest is meaningless (the coordinator cannot verify
	// the member converged to the right artifact) and is rejected (I5 guard).
	_, err := mustPropose(t).WithAck("node-a", "", time.Unix(1, 0))
	if !errors.Is(err, shared.ErrRolloutAckRejected) {
		t.Fatalf("empty-digest ack err = %v, want ErrRolloutAckRejected", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// WithCommit (I2: requires Staging + all acks; I3: fencing; I4: terminal)
// ─────────────────────────────────────────────────────────────────────────

func TestRollout_Commit_RequiresAllAcks(t *testing.T) {
	r := mustPropose(t)
	r, _ = r.WithAck("node-a", "b", time.Unix(1, 0)) // only 1 of 3
	if r.CanCommit() {
		t.Fatal("CanCommit() true with incomplete acks")
	}
	_, err := r.WithCommit(tok(1))
	if !errors.Is(err, shared.ErrRolloutNotCommittable) {
		t.Fatalf("err = %v, want ErrRolloutNotCommittable", err)
	}
}

func TestRollout_Commit_FromProposedRejected(t *testing.T) {
	// A Proposed rollout (no acks yet) is not committable: exercises the
	// Proposed branch of the I2 barrier check, distinct from Staging+incomplete.
	r := mustPropose(t)
	if r.State() != persistence.RolloutProposed || r.CanCommit() {
		t.Fatalf("precondition: want Proposed & !CanCommit, got %q canCommit=%v", r.State(), r.CanCommit())
	}
	_, err := r.WithCommit(tok(1))
	if !errors.Is(err, shared.ErrRolloutNotCommittable) {
		t.Fatalf("commit-from-proposed err = %v, want ErrRolloutNotCommittable", err)
	}
}

func TestRollout_Commit_Succeeds(t *testing.T) {
	r := mustStaged(t)
	if !r.CanCommit() {
		t.Fatal("CanCommit() false after all acks")
	}
	c, err := r.WithCommit(tok(3))
	if err != nil {
		t.Fatalf("WithCommit: %v", err)
	}
	if c.State() != persistence.RolloutCommitted {
		t.Fatalf("state = %q, want committed", c.State())
	}
	if c.CoordinatorVersion() != 3 {
		t.Fatalf("coordinator version = %d, want 3", c.CoordinatorVersion())
	}
}

func TestRollout_Commit_Idempotent(t *testing.T) {
	c, err := mustStaged(t).WithCommit(tok(2))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Re-commit with the same (or newer) token is a no-op success.
	c2, err := c.WithCommit(tok(2))
	if err != nil {
		t.Fatalf("idempotent re-commit err = %v", err)
	}
	if c2.State() != persistence.RolloutCommitted {
		t.Fatalf("state = %q, want committed", c2.State())
	}
}

func TestRollout_Commit_NewerTokenIdempotentKeepsFence(t *testing.T) {
	// A NEWER coordinator re-committing an already-committed rollout is an
	// idempotent no-op success (design goal G3, "same-or-newer token").
	c, _ := mustStaged(t).WithCommit(tok(2))
	c2, err := c.WithCommit(tok(3))
	if err != nil {
		t.Fatalf("newer-token re-commit err = %v", err)
	}
	if c2.State() != persistence.RolloutCommitted {
		t.Fatalf("state = %q, want committed", c2.State())
	}
	// The no-op must NOT advance the fence high-water mark: idempotency means no
	// state change, and the fence is inert on a terminal (immutable) rollout —
	// the next generation resets it to 0 regardless. Pins the deliberate choice.
	if c2.CoordinatorVersion() != 2 {
		t.Fatalf("coordinator version = %d, want 2 (idempotent no-op must not advance the fence)", c2.CoordinatorVersion())
	}
}

func TestRollout_Commit_StaleTokenRejected(t *testing.T) {
	c, _ := mustStaged(t).WithCommit(tok(5)) // coordinator version now 5
	_, err := c.WithCommit(tok(4))           // deposed coordinator, older token
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("err = %v, want ErrStaleFencingToken", err)
	}
}

func TestRollout_Commit_InvalidTokenRejected(t *testing.T) {
	r := mustStaged(t)
	for _, bad := range []persistence.LeaseToken{{}, {Version: 1}, {Owner: "x"}} {
		if _, err := r.WithCommit(bad); !errors.Is(err, shared.ErrStaleFencingToken) {
			t.Fatalf("commit with invalid token %+v: err = %v, want ErrStaleFencingToken", bad, err)
		}
	}
}

func TestRollout_Commit_OnAbortedRejected(t *testing.T) {
	a, _ := mustPropose(t).WithAbort(tok(1), "operator")
	_, err := a.WithCommit(tok(2))
	if !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("commit-of-aborted err = %v, want ErrRolloutTerminal", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// WithAbort (I3: fencing; I4: terminal)
// ─────────────────────────────────────────────────────────────────────────

func TestRollout_Abort_FromProposedOrStaging(t *testing.T) {
	for _, r := range []persistence.Rollout{mustPropose(t), mustStaged(t)} {
		a, err := r.WithAbort(tok(1), "deadline")
		if err != nil {
			t.Fatalf("WithAbort: %v", err)
		}
		if a.State() != persistence.RolloutAborted || a.Reason() != "deadline" {
			t.Fatalf("state/reason = %q/%q", a.State(), a.Reason())
		}
	}
}

func TestRollout_Abort_Idempotent(t *testing.T) {
	a, _ := mustPropose(t).WithAbort(tok(2), "x")
	a2, err := a.WithAbort(tok(2), "y")
	if err != nil {
		t.Fatalf("idempotent re-abort err = %v", err)
	}
	if a2.State() != persistence.RolloutAborted {
		t.Fatalf("state = %q, want aborted", a2.State())
	}
}

func TestRollout_Abort_NewerTokenIdempotentKeepsFence(t *testing.T) {
	a, _ := mustPropose(t).WithAbort(tok(2), "x")
	a2, err := a.WithAbort(tok(9), "y")
	if err != nil {
		t.Fatalf("newer-token re-abort err = %v", err)
	}
	if a2.State() != persistence.RolloutAborted || a2.CoordinatorVersion() != 2 {
		t.Fatalf("state/coordVersion = %q/%d, want aborted/2", a2.State(), a2.CoordinatorVersion())
	}
}

func TestRollout_Abort_StaleTokenRejected(t *testing.T) {
	a, _ := mustPropose(t).WithAbort(tok(5), "x")
	_, err := a.WithAbort(tok(4), "y")
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("err = %v, want ErrStaleFencingToken", err)
	}
}

func TestRollout_Abort_OnCommittedRejected(t *testing.T) {
	c, _ := mustStaged(t).WithCommit(tok(1))
	_, err := c.WithAbort(tok(2), "x")
	if !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("abort-of-committed err = %v, want ErrRolloutTerminal", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// WithNack (F2 input; coordinator later aborts)
// ─────────────────────────────────────────────────────────────────────────

func TestRollout_Nack_RecordsWithoutTerminating(t *testing.T) {
	r, err := mustPropose(t).WithNack("node-b", "plugin missing")
	if err != nil {
		t.Fatalf("WithNack: %v", err)
	}
	if r.State().IsTerminal() {
		t.Fatal("nack must not terminate the rollout (coordinator aborts)")
	}
	if r.Nacks()["node-b"] != "plugin missing" {
		t.Fatalf("nack not recorded: %+v", r.Nacks())
	}
}

func TestRollout_Nack_Rejections(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) persistence.Rollout
		mem   string
	}{
		{"member_not_in_epoch", mustPropose, "stranger"},
		{"already_acked", func(t *testing.T) persistence.Rollout {
			r := mustPropose(t)
			r, _ = r.WithAck("node-a", "b", time.Unix(1, 0))
			return r
		}, "node-a"},
		{"already_nacked", func(t *testing.T) persistence.Rollout {
			r := mustPropose(t)
			r, _ = r.WithNack("node-a", "bad")
			return r
		}, "node-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.setup(t).WithNack(tc.mem, "reason")
			if !errors.Is(err, shared.ErrRolloutAckRejected) {
				t.Fatalf("err = %v, want ErrRolloutAckRejected", err)
			}
		})
	}
}

func TestRollout_Nack_OnTerminalRejected(t *testing.T) {
	a, _ := mustPropose(t).WithAbort(tok(1), "operator")
	_, err := a.WithNack("node-a", "late")
	if !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("nack-after-abort err = %v, want ErrRolloutTerminal", err)
	}
}

// TestRolloutFencingIsRejectionOfReDecisionOnly pins the ACTUAL reach of
// invariant I3, which is narrower than "a deposed coordinator cannot flip
// state": coordVersion is the version of the token that LAST DECIDED, and it is
// zero while the rollout is non-terminal. So before any decision exists, every
// valid token passes the fence — including a deposed coordinator's.
//
// This is pinned, not fixed, because closing it needs the rollout row to learn
// the live coordinator epoch BEFORE a decision (an explicit claim/heartbeat
// write, a protocol addition, not a domain tweak). The residual is fail-SAFE and
// bounded:
//   - a zombie Commit still requires the full ack barrier (I2) — it can only do
//     what the live coordinator was about to do;
//   - a zombie Abort leaves the OLD config serving (nothing swaps), costing the
//     operator a retry, not correctness;
//   - bridge.firstSideEffectAllowed makes a successor wait one full lease TTL
//     before its first side effect, so a zombie must be more than a lease TTL
//     stale to still be acting at all.
//
// Once a decision exists, the fence does bite — the second half of this test.
func TestRolloutFencingIsRejectionOfReDecisionOnly(t *testing.T) {
	live := persistence.LeaseToken{Owner: "node-live", Version: 9}
	deposed := persistence.LeaseToken{Owner: "node-zombie", Version: 2}

	// Before any decision: the deposed token is NOT rejected.
	r, err := persistence.NewRollout(1, persistence.RolloutProposal{
		ProposerID: "node-live", ConfigDigest: "d", Members: []string{"node-a"}, TTL: time.Minute,
	}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("persistence.NewRollout: %v", err)
	}
	if r.CoordinatorVersion() != 0 {
		t.Fatalf("a fresh rollout must record no coordinator epoch, got %d", r.CoordinatorVersion())
	}
	aborted, err := r.WithAbort(deposed, "zombie abort")
	if err != nil {
		t.Fatalf("documented gap: a pre-decision abort by a deposed coordinator is currently "+
			"ACCEPTED; if this now errors the fence was strengthened — update this test and the "+
			"design doc F5 row: %v", err)
	}
	if aborted.State() != persistence.RolloutAborted {
		t.Fatalf("state = %q, want aborted", aborted.State())
	}

	// After a decision: the fence bites — the live coordinator cannot re-decide
	// across directions, and a token older than the deciding one is stale.
	if _, err := aborted.WithCommit(live); err == nil {
		t.Fatal("commit of an aborted rollout must be rejected (I4)")
	}
	older := persistence.LeaseToken{Owner: "node-older", Version: 1}
	if _, err := aborted.WithAbort(older, "older"); err == nil {
		t.Fatal("an abort carrying a token below the deciding version must be stale-rejected (I3)")
	}
}

// TestRolloutAckIsNotIdempotent pins invariant I5 as a STRICT at-most-once vote:
// a member re-acking the same generation with the same build digest is rejected,
// not silently accepted. A member whose Ack response was lost therefore MUST
// recover by reading Current (its own ack is visible there) rather than blindly
// retrying the write — the applier loop's contract, recorded here so a later
// phase does not mistake the rejection for a wedge.
func TestRolloutAckIsNotIdempotent(t *testing.T) {
	r, err := persistence.NewRollout(1, persistence.RolloutProposal{
		ProposerID: "c", ConfigDigest: "d", Members: []string{"node-a"}, TTL: time.Minute,
	}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("persistence.NewRollout: %v", err)
	}
	acked, err := r.WithAck("node-a", "build:1", time.Now())
	if err != nil {
		t.Fatalf("first ack: %v", err)
	}
	if _, err := acked.WithAck("node-a", "build:1", time.Now()); err == nil {
		t.Fatal("a repeated ack must be rejected (I5); recover by reading Current, not by retrying")
	}
	if _, ok := acked.Acks()["node-a"]; !ok {
		t.Fatal("the recorded ack must remain readable so a retrying member can self-diagnose")
	}
}

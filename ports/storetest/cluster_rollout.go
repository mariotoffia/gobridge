package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// RunClusterRolloutStoreTests executes the full conformance suite against a
// ClusterRolloutStore. Because a rollout store is a SINGLE-POINT coordinator
// (no per-key isolation -- one active rollout at a time), the caller supplies a
// factory that returns a FRESH, empty store; each subtest builds its own so
// they never interfere.
func RunClusterRolloutStoreTests(t *testing.T, newStore func() ports.ClusterRolloutStore) {
	t.Helper()

	t.Run("CurrentEmptyNotFound", func(t *testing.T) { rolloutCurrentEmpty(t, newStore()) })
	t.Run("ProposeCreatesProposed", func(t *testing.T) { rolloutProposeCreates(t, newStore()) })
	t.Run("DoubleProposeConflicts", func(t *testing.T) { rolloutDoublePropose(t, newStore()) })
	t.Run("AckAdvancesToStaging", func(t *testing.T) { rolloutAckStaging(t, newStore()) })
	t.Run("AckUnknownGenerationNotFound", func(t *testing.T) { rolloutAckUnknownGen(t, newStore()) })
	t.Run("AckStrangerRejected", func(t *testing.T) { rolloutAckStranger(t, newStore()) })
	t.Run("AckTwiceRejected", func(t *testing.T) { rolloutAckTwice(t, newStore()) })
	t.Run("CommitRequiresAllAcks", func(t *testing.T) { rolloutCommitIncomplete(t, newStore()) })
	t.Run("CommitAfterAllAcks", func(t *testing.T) { rolloutCommitComplete(t, newStore()) })
	t.Run("CommitStaleTokenRejected", func(t *testing.T) { rolloutCommitStale(t, newStore()) })
	t.Run("CommitIdempotent", func(t *testing.T) { rolloutCommitIdempotent(t, newStore()) })
	t.Run("AbortFromStaging", func(t *testing.T) { rolloutAbort(t, newStore()) })
	t.Run("AbortStaleTokenRejected", func(t *testing.T) { rolloutAbortStale(t, newStore()) })
	t.Run("AbortIdempotent", func(t *testing.T) { rolloutAbortIdempotent(t, newStore()) })
	t.Run("CommitOfAbortedRejected", func(t *testing.T) { rolloutCommitOfAborted(t, newStore()) })
	t.Run("AckAfterAbortRejected", func(t *testing.T) { rolloutAckAfterAbort(t, newStore()) })
	t.Run("TerminalImmutable", func(t *testing.T) { rolloutTerminalImmutable(t, newStore()) })
	t.Run("NackThenAbort", func(t *testing.T) { rolloutNackThenAbort(t, newStore()) })
	t.Run("ProposeAfterTerminalNewGeneration", func(t *testing.T) { rolloutProposeAfterTerminal(t, newStore()) })
	t.Run("CurrentReturnsSnapshot", func(t *testing.T) { rolloutCurrentSnapshot(t, newStore()) })
	t.Run("ConcurrentCommitAbortAtomic", func(t *testing.T) { rolloutConcurrentCommitAbort(t, newStore()) })
	t.Run("ConcurrentAcksNoLostUpdate", func(t *testing.T) { rolloutConcurrentAcks(t, newStore()) })
	t.Run("ProposeMalformedRejected", func(t *testing.T) { rolloutProposeMalformed(t, newStore()) })
	t.Run("MutateUnknownGenerationNotFound", func(t *testing.T) { rolloutMutateUnknownGen(t, newStore()) })
	t.Run("ConcurrentProposeSingleWinner", func(t *testing.T) { rolloutConcurrentPropose(t, newStore()) })
}

// ── helpers ──────────────────────────────────────────────────────────────

func rolloutProposal(members ...string) persistence.RolloutProposal {
	return persistence.RolloutProposal{
		ProposerID:    "coordinator",
		ConfigDigest:  "sha256:cafef00d",
		ConfigVersion: 42,
		Members:       members,
		TTL:           5 * time.Minute,
	}
}

func coordToken(v uint64) persistence.LeaseToken {
	return persistence.LeaseToken{Version: v, Owner: "coordinator"}
}

// current reads the store's current rollout, failing the test on error.
func current(t *testing.T, store ports.ClusterRolloutStore) persistence.Rollout {
	t.Helper()
	r, err := store.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	return r
}

// proposeGen proposes members and returns the new rollout's generation.
func proposeGen(t *testing.T, store ports.ClusterRolloutStore, members ...string) uint64 {
	t.Helper()
	r, err := store.Propose(context.Background(), rolloutProposal(members...))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return r.Generation()
}

// stageAll proposes members and acks every one of them, returning the
// commit-ready (Staging, all acked) rollout's generation.
func stageAll(t *testing.T, store ports.ClusterRolloutStore, members ...string) uint64 {
	t.Helper()
	ctx := context.Background()
	gen := proposeGen(t, store, members...)
	for _, m := range members {
		if err := store.Ack(ctx, gen, m, "build:"+m); err != nil {
			t.Fatalf("Ack(%s): %v", m, err)
		}
	}
	return gen
}

// ── subtests ─────────────────────────────────────────────────────────────

func rolloutCurrentEmpty(t *testing.T, store ports.ClusterRolloutStore) {
	if _, err := store.Current(context.Background()); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("Current on empty store: err = %v, want ErrNotFound", err)
	}
}

func rolloutProposeCreates(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	r, err := store.Propose(ctx, rolloutProposal("node-a", "node-b"))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if r.State() != persistence.RolloutProposed {
		t.Fatalf("state = %q, want proposed", r.State())
	}
	if r.Generation() == 0 {
		t.Fatal("generation must be > 0")
	}
	if cur := current(t, store); cur.Generation() != r.Generation() {
		t.Fatalf("Current generation = %d, want %d", cur.Generation(), r.Generation())
	}
}

func rolloutDoublePropose(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	if _, err := store.Propose(ctx, rolloutProposal("node-a")); err != nil {
		t.Fatalf("first Propose: %v", err)
	}
	_, err := store.Propose(ctx, rolloutProposal("node-a"))
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("second Propose while active: err = %v, want ErrAlreadyExists", err)
	}
}

func rolloutAckStaging(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a", "node-b")
	if err := store.Ack(ctx, gen, "node-a", "build:1"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	cur := current(t, store)
	if cur.State() != persistence.RolloutStaging {
		t.Fatalf("state = %q, want staging", cur.State())
	}
	if _, ok := cur.Acks()["node-a"]; !ok {
		t.Fatalf("ack not recorded: %+v", cur.Acks())
	}
}

func rolloutAckUnknownGen(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a")
	if err := store.Ack(ctx, gen+1, "node-a", "build:1"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("Ack on unknown generation: err = %v, want ErrNotFound", err)
	}
}

func rolloutAckStranger(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a")
	if err := store.Ack(ctx, gen, "stranger", "build:1"); !errors.Is(err, shared.ErrRolloutAckRejected) {
		t.Fatalf("Ack from non-epoch member: err = %v, want ErrRolloutAckRejected", err)
	}
}

func rolloutAckTwice(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a", "node-b")
	if err := store.Ack(ctx, gen, "node-a", "build:1"); err != nil {
		t.Fatalf("first Ack: %v", err)
	}
	if err := store.Ack(ctx, gen, "node-a", "build:1"); !errors.Is(err, shared.ErrRolloutAckRejected) {
		t.Fatalf("second Ack from same member: err = %v, want ErrRolloutAckRejected", err)
	}
}

func rolloutCommitIncomplete(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a", "node-b")
	if err := store.Ack(ctx, gen, "node-a", "build:1"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := store.Commit(ctx, gen, coordToken(1)); !errors.Is(err, shared.ErrRolloutNotCommittable) {
		t.Fatalf("Commit with incomplete acks: err = %v, want ErrRolloutNotCommittable", err)
	}
	// state must be unchanged (still Staging) after the rejected commit.
	if cur := current(t, store); cur.State() != persistence.RolloutStaging {
		t.Fatalf("state after rejected commit = %q, want staging", cur.State())
	}
}

func rolloutCommitComplete(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := stageAll(t, store, "node-a", "node-b")
	if err := store.Commit(ctx, gen, coordToken(3)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	cur := current(t, store)
	if cur.State() != persistence.RolloutCommitted {
		t.Fatalf("state = %q, want committed", cur.State())
	}
	if cur.CoordinatorVersion() != 3 {
		t.Fatalf("coordinator version = %d, want 3", cur.CoordinatorVersion())
	}
}

func rolloutCommitStale(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := stageAll(t, store, "node-a")
	if err := store.Commit(ctx, gen, coordToken(5)); err != nil {
		t.Fatalf("Commit v5: %v", err)
	}
	if err := store.Commit(ctx, gen, coordToken(4)); !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("stale Commit: err = %v, want ErrStaleFencingToken", err)
	}
	if cur := current(t, store); cur.CoordinatorVersion() != 5 {
		t.Fatalf("coordinator version after stale commit = %d, want 5 (unchanged)", cur.CoordinatorVersion())
	}
}

func rolloutCommitIdempotent(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := stageAll(t, store, "node-a")
	if err := store.Commit(ctx, gen, coordToken(2)); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := store.Commit(ctx, gen, coordToken(2)); err != nil { // resume after crash
		t.Fatalf("idempotent re-Commit: err = %v", err)
	}
	if cur := current(t, store); cur.State() != persistence.RolloutCommitted {
		t.Fatalf("state = %q, want committed", cur.State())
	}
}

func rolloutAbort(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a", "node-b")
	if err := store.Abort(ctx, gen, coordToken(1), "deadline exceeded"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	cur := current(t, store)
	if cur.State() != persistence.RolloutAborted || cur.Reason() != "deadline exceeded" {
		t.Fatalf("state/reason = %q/%q", cur.State(), cur.Reason())
	}
}

func rolloutAbortStale(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a")
	if err := store.Abort(ctx, gen, coordToken(5), "x"); err != nil {
		t.Fatalf("Abort v5: %v", err)
	}
	if err := store.Abort(ctx, gen, coordToken(4), "y"); !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("stale Abort: err = %v, want ErrStaleFencingToken", err)
	}
	// the rejected abort must not have mutated state (reason/coordVersion intact).
	if cur := current(t, store); cur.CoordinatorVersion() != 5 || cur.Reason() != "x" {
		t.Fatalf("state after stale abort: coordVersion=%d reason=%q, want 5/%q (unchanged)",
			cur.CoordinatorVersion(), cur.Reason(), "x")
	}
}

func rolloutAbortIdempotent(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a")
	if err := store.Abort(ctx, gen, coordToken(2), "deadline"); err != nil {
		t.Fatalf("first Abort: %v", err)
	}
	if err := store.Abort(ctx, gen, coordToken(2), "deadline"); err != nil { // resume after crash
		t.Fatalf("idempotent re-Abort: err = %v", err)
	}
	if cur := current(t, store); cur.State() != persistence.RolloutAborted {
		t.Fatalf("state = %q, want aborted", cur.State())
	}
}

func rolloutCommitOfAborted(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := stageAll(t, store, "node-a")
	if err := store.Abort(ctx, gen, coordToken(1), "operator"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// commit-of-aborted is the terminal-immutable direction opposite to
	// TerminalImmutable's abort-of-committed (I4).
	if err := store.Commit(ctx, gen, coordToken(2)); !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("Commit of aborted: err = %v, want ErrRolloutTerminal", err)
	}
}

func rolloutAckAfterAbort(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a", "node-b")
	if err := store.Abort(ctx, gen, coordToken(1), "operator"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := store.Ack(ctx, gen, "node-a", "build:1"); !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("Ack after abort: err = %v, want ErrRolloutTerminal", err)
	}
}

func rolloutTerminalImmutable(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := stageAll(t, store, "node-a")
	if err := store.Commit(ctx, gen, coordToken(1)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// abort-of-committed must be rejected (I4).
	if err := store.Abort(ctx, gen, coordToken(2), "x"); !errors.Is(err, shared.ErrRolloutTerminal) {
		t.Fatalf("Abort of committed: err = %v, want ErrRolloutTerminal", err)
	}
}

func rolloutNackThenAbort(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a", "node-b")
	if err := store.Nack(ctx, gen, "node-b", "plugin missing"); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	n := current(t, store)
	if n.State().IsTerminal() {
		t.Fatal("nack must not terminate the rollout")
	}
	if n.Nacks()["node-b"] != "plugin missing" {
		t.Fatalf("nack not recorded: %+v", n.Nacks())
	}
	// coordinator aborts in response to the nack.
	if err := store.Abort(ctx, gen, coordToken(1), "member nacked"); err != nil {
		t.Fatalf("Abort after nack: %v", err)
	}
	if cur := current(t, store); cur.State() != persistence.RolloutAborted {
		t.Fatalf("state = %q, want aborted", cur.State())
	}
}

func rolloutProposeAfterTerminal(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen1 := stageAll(t, store, "node-a")
	if err := store.Commit(ctx, gen1, coordToken(1)); err != nil {
		t.Fatalf("Commit gen1: %v", err)
	}
	r2, err := store.Propose(ctx, rolloutProposal("node-a", "node-b"))
	if err != nil {
		t.Fatalf("Propose after terminal: %v", err)
	}
	if r2.Generation() <= gen1 {
		t.Fatalf("new generation %d must exceed prior %d", r2.Generation(), gen1)
	}
	if r2.State() != persistence.RolloutProposed {
		t.Fatalf("state = %q, want proposed", r2.State())
	}
}

func rolloutCurrentSnapshot(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := proposeGen(t, store, "node-a", "node-b")
	if err := store.Ack(ctx, gen, "node-a", "build:1"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	cur := current(t, store)
	// Mutating the returned snapshot must not corrupt store state.
	cur.Acks()["ghost"] = persistence.RolloutAck{MemberID: "ghost"}
	cur.MembershipEpoch()[0] = "hacked"
	again := current(t, store)
	if _, ok := again.Acks()["ghost"]; ok {
		t.Fatal("store returned an aliased Acks map")
	}
	for _, m := range again.MembershipEpoch() {
		if m == "hacked" {
			t.Fatal("store returned an aliased MembershipEpoch slice")
		}
	}
}

// rolloutConcurrentCommitAbort proves the terminal decision is atomic: many
// goroutines race Commit against Abort with the same fencing token; the store
// must settle on exactly ONE direction -- never both.
func rolloutConcurrentCommitAbort(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	gen := stageAll(t, store, "node-a")

	const n = 8
	var wg sync.WaitGroup
	var commits, aborts atomic.Int64
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if err := store.Commit(ctx, gen, coordToken(1)); err == nil {
					commits.Add(1)
				}
			} else {
				if err := store.Abort(ctx, gen, coordToken(1), "race"); err == nil {
					aborts.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	if commits.Load() > 0 && aborts.Load() > 0 {
		t.Fatalf("both directions succeeded (commits=%d aborts=%d): terminal decision not atomic",
			commits.Load(), aborts.Load())
	}
	cur := current(t, store)
	if !cur.State().IsTerminal() {
		t.Fatalf("state after race = %q, want terminal", cur.State())
	}
	wantCommitted := commits.Load() > 0
	if wantCommitted && cur.State() != persistence.RolloutCommitted {
		t.Fatalf("commits won but state = %q", cur.State())
	}
	if !wantCommitted && cur.State() != persistence.RolloutAborted {
		t.Fatalf("aborts won but state = %q", cur.State())
	}
}

// rolloutConcurrentAcks proves concurrent acks from distinct members are not
// lost under the store's compare-and-set.
func rolloutConcurrentAcks(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	const n = 8
	members := make([]string, n)
	for i := range members {
		members[i] = fmt.Sprintf("node-%d", i)
	}
	gen := proposeGen(t, store, members...)

	var wg sync.WaitGroup
	wg.Add(n)
	for _, m := range members {
		go func(m string) {
			defer wg.Done()
			if err := store.Ack(ctx, gen, m, "build:"+m); err != nil {
				t.Errorf("Ack(%s): %v", m, err)
			}
		}(m)
	}
	wg.Wait()

	cur := current(t, store)
	if len(cur.Acks()) != n {
		t.Fatalf("acks recorded = %d, want %d (lost update under concurrency)", len(cur.Acks()), n)
	}
	if !cur.CanCommit() {
		t.Fatal("barrier not satisfied after all concurrent acks landed")
	}
}

// rolloutProposeMalformed proves the store surfaces the domain's proposal
// validation (the suite is the store's behavioral spec).
func rolloutProposeMalformed(t *testing.T, store ports.ClusterRolloutStore) {
	_, err := store.Propose(context.Background(), rolloutProposal()) // no members
	if !errors.Is(err, shared.ErrInvalidRolloutProposal) {
		t.Fatalf("malformed Propose: err = %v, want ErrInvalidRolloutProposal", err)
	}
}

// rolloutMutateUnknownGen proves every mutator (not just Ack) rejects a
// generation that is not the active rollout with ErrNotFound (port contract).
func rolloutMutateUnknownGen(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	stale := proposeGen(t, store, "node-a") + 1
	if err := store.Nack(ctx, stale, "node-a", "x"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("Nack unknown gen: err = %v, want ErrNotFound", err)
	}
	if err := store.Commit(ctx, stale, coordToken(1)); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("Commit unknown gen: err = %v, want ErrNotFound", err)
	}
	if err := store.Abort(ctx, stale, coordToken(1), "x"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("Abort unknown gen: err = %v, want ErrNotFound", err)
	}
}

// rolloutConcurrentPropose proves I1's split-brain guard (design F4): many
// goroutines racing to open a rollout on an empty store yield exactly ONE
// winner; the rest get ErrAlreadyExists and no second active rollout exists.
func rolloutConcurrentPropose(t *testing.T, store ports.ClusterRolloutStore) {
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	var winners, conflicts atomic.Int64
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			switch _, err := store.Propose(ctx, rolloutProposal("node-a")); {
			case err == nil:
				winners.Add(1)
			case errors.Is(err, shared.ErrAlreadyExists):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected Propose error: %v", err)
			}
		}()
	}
	wg.Wait()

	if winners.Load() != 1 {
		t.Fatalf("Propose winners = %d, want exactly 1 (I1: single active rollout)", winners.Load())
	}
	if conflicts.Load() != n-1 {
		t.Fatalf("Propose conflicts = %d, want %d", conflicts.Load(), n-1)
	}
	if cur := current(t, store); cur.State() != persistence.RolloutProposed {
		t.Fatalf("state after concurrent propose = %q, want proposed", cur.State())
	}
}

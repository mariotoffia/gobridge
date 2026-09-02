package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Getting a member back in step with the cohort after the barrier has decided.
//
// The three local repairs — reaching the decided generation, recording the
// durable artifact, and reverting a provisional one — share a bound and a
// terminal latch, and the interesting cases are where they CROSS: a member that
// cannot apply also owes a revert; a member whose revert failed is then
// confirmed on the generation it never left. Each of these pins one crossing
// that produced either a permanently split cohort or a terminal signal outliving
// its own condition.

// TestClusterRolloutApply_DeclaresAnUnapplicableCommitTerminal is the
// never-hidden half of the convergence contract. The cohort has already
// committed, so a member whose swap keeps failing runs an OLDER generation than
// its peers while the shared row reads "committed" on every member — the split
// is invisible in every signal that describes the row. Once the bounded repair
// is exhausted the member must declare itself unable to reach the cohort's
// decision, which is the signal that gets it replaced.
func TestClusterRolloutApply_DeclaresAnUnapplicableCommitTerminal(t *testing.T) {
	store := memoryrollout.NewStore()
	boot := soloCohortConfig(0)
	host := newFakeRolloutHost(boot)
	host.refuse[7] = -1 // every swap to the candidate fails and restores the old config

	d := NewClusterRolloutDriver(host, fastRolloutConfig(store, "node-a"))
	require.NotNil(t, d)
	stop := d.Start(context.Background(), clock.System, nil)
	defer stop(context.Background())

	candidate := soloCohortConfig(7)
	candidate.Bindings[0].Address = "addr/rolled"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))

	wait.Until(t, 10*time.Second, "the member declares the unapplicable commit terminal", func() bool {
		st, ok := d.Status()
		return ok && st.TerminalGeneration == 1
	})

	st, ok := d.Status()
	require.True(t, ok)
	assert.False(t, st.Applied, "committed AND not applied is the split this signal exists to expose")
	assert.Contains(t, st.TerminalReason, "replace",
		"a member that cannot reach the committed generation is repaired by replacing it")
	assert.NotEmpty(t, host.degradedReason(), "the divergence must also degrade this member's health")
}

// TestClusterRolloutApply_ConvergesOnceTheApplyCauseClears is the never-permanent
// half. Bounding the retries stops a deterministic failure from rebuilding the
// runtime every poll, but a bound that also stops TRYING turns a ten-minute
// broker outage into a permanently split cohort no operator was asked about.
// The repair therefore keeps going at the capped backoff, and converging retracts
// the terminal latch.
func TestClusterRolloutApply_ConvergesOnceTheApplyCauseClears(t *testing.T) {
	store := memoryrollout.NewStore()
	boot := soloCohortConfig(0)
	host := newFakeRolloutHost(boot)
	host.refuse[7] = -1

	d := NewClusterRolloutDriver(host, fastRolloutConfig(store, "node-a"))
	require.NotNil(t, d)
	stop := d.Start(context.Background(), clock.System, nil)
	defer stop(context.Background())

	candidate := soloCohortConfig(7)
	candidate.Bindings[0].Address = "addr/rolled"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))
	wait.Until(t, 10*time.Second, "the member exhausts its bounded apply repair", func() bool {
		st, ok := d.Status()
		return ok && st.TerminalGeneration == 1
	})

	host.allow(7) // the transient cause clears

	wait.Until(t, 20*time.Second, "the member converges to the committed generation on its own", func() bool {
		st, ok := d.Status()
		return ok && st.Applied && st.TerminalGeneration == 0
	})
	assert.Equal(t, 7, host.Config().Version)
}

// TestClusterRolloutApply_ATerminalSwapStillLetsTheDeadmanAbandonTheGeneration
// is the confirm-window half of the terminal-apply contract, and the one with
// teeth: it is about a member REJOINING a generation the cohort walked away
// from.
//
// A member whose provisional swap keeps failing latches terminal at that
// generation. The local deadman must still be able to record the generation as
// reverted when the confirm window expires — otherwise the member goes on
// chasing a provisional generation whose window closed, records convergence for
// it if the cause ever clears, and swaps to config the cohort abandoned.
func TestClusterRolloutApply_ATerminalSwapStillLetsTheDeadmanAbandonTheGeneration(t *testing.T) {
	ctx := context.Background()
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	store := memoryrollout.NewStore(memoryrollout.WithClock(fake))

	boot := soloWindowConfig(0, time.Second)
	boot.Bindings[0].Address = "addr/original"
	host := newFakeRolloutHost(boot)
	host.refuse[7] = -1 // every provisional swap fails

	candidate := soloWindowConfig(7, time.Second)
	candidate.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(candidate)
	require.True(t, ok)

	barrier := &rolloutBarrier{store: store, memberID: "node-a", pollInterval: time.Second, ops: newRolloutOps(0)}
	barrier.stage(digest, candidate, candidate)
	applier := &rolloutApplier{
		host: host, barrier: barrier, store: store, memberID: "node-a",
		clk: fake, obs: newRolloutObserver(nil, fake, time.Second, "node-a"),
	}
	seedWindowedCommit(t, store, digest, 7, time.Second)

	for range maxAdoptAttempts {
		require.NoError(t, applier.tick(ctx))
	}
	require.Equal(t, uint64(1), applier.terminalGen, "precondition: the swap is terminal")
	require.Equal(t, rolloutRepairApply, applier.terminalClass)

	// The coordinator is dead, so nothing writes Reverted: the row stays
	// Committed and only this member's own deadman can end the window.
	fake.Advance(time.Hour)
	require.NoError(t, applier.tick(ctx))
	assert.Equal(t, uint64(1), applier.revertedGen,
		"the deadman must abandon the generation even though the swap is terminal")

	// Hours later the cause clears. The generation is gone; it must not be joined.
	host.allow(7)
	for range 40 {
		fake.Advance(time.Minute)
		require.NoError(t, applier.tick(ctx))
	}
	assert.Equal(t, 0, host.Config().Version,
		"a member past its own deadman must never swap to the abandoned generation")
	cur, err := store.Current(ctx)
	require.NoError(t, err)
	assert.NotContains(t, cur.Converged(), "node-a",
		"nor record convergence for it, which would let the coordinator confirm a lapsed window")
}

// TestClusterRolloutApply_DoesNotThrashBetweenASupersededArtifactAndTheDecidedRow
// pins the pacing across the TWO paths that can apply.
//
// A member can owe convergence to the active row's generation and, at the same
// time, hold a durable artifact at an older one a peer wrote. The bounded retry
// is keyed on the generation, so a member that alternated between the two paths
// would reset its own backoff on every poll and rebuild the runtime forever —
// the self-inflicted outage the bound exists to prevent, reached by the one route
// the bound does not see.
func TestClusterRolloutApply_DoesNotThrashBetweenASupersededArtifactAndTheDecidedRow(t *testing.T) {
	ctx := context.Background()
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	store := memoryrollout.NewStore(memoryrollout.WithClock(fake))
	codec := newConfigCodecFake()

	host := newFakeRolloutHost(soloCohortConfig(0))
	older := soloCohortConfig(5) // a peer committed this and wrote the artifact
	older.Bindings[0].Address = "addr/older"
	newer := soloCohortConfig(9) // the roll-forward this member staged
	newer.Bindings[0].Address = "addr/newer"
	host.refuse[5], host.refuse[9] = -1, -1 // neither can be applied here

	olderBytes := codec.register(older)
	olderDigest, ok := configCanonicalBytesDigest(older)
	require.True(t, ok)
	newerDigest, ok := configCanonicalBytesDigest(newer)
	require.True(t, ok)

	barrier := &rolloutBarrier{
		store: store, committedStore: store, memberID: "node-a",
		pollInterval: time.Second, ops: newRolloutOps(0),
		encode: codec.encode, decode: codec.decode,
	}
	barrier.stage(newerDigest, newer, newer)
	applier := &rolloutApplier{
		host: host, barrier: barrier, store: store, memberID: "node-a",
		clk: fake, obs: newRolloutObserver(nil, fake, time.Second, "node-a"),
	}

	require.NoError(t, store.PutCommittedConfig(ctx, persistence.CommittedRolloutConfig{
		Generation: 1, ConfigVersion: 5, ConfigBytes: olderBytes, Digest: olderDigest,
	}))
	seedCommit(t, store, olderDigest, 5)
	seedCommit(t, store, newerDigest, 9)

	for range 60 {
		fake.Advance(time.Second)
		require.NoError(t, applier.tick(ctx))
	}

	assert.Less(t, host.appliedCount(), 15,
		"past the bound the capped backoff must hold, whichever path drives the apply")
}

// TestClusterRolloutRepair_ACompletedRevertLeavesTheArtifactLatchStanding pins
// which repair may retract which latch.
//
// The two latches mean opposite things — one says this member is running config
// the cohort REJECTED, the other that it cannot record what it would BOOT on —
// and a corrupt artifact is never fixed by a revert succeeding. Retracting it
// there silences the only signal that names an unbootable member.
func TestClusterRolloutRepair_ACompletedRevertLeavesTheArtifactLatchStanding(t *testing.T) {
	ctx := context.Background()
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	boot := soloCohortConfig(0)
	host := newFakeRolloutHost(boot)
	applier := &rolloutApplier{
		host: host, memberID: "node-a", clk: fake,
		obs: newRolloutObserver(nil, fake, time.Second, "node-a"),
	}

	// The artifact at generation 1 holds a different config: unrepairable, so the
	// repair is dropped and the latch is all that is left of it.
	applier.markUnsafe(1, rolloutRepairArtifact,
		"the durable artifact holds a DIFFERENT config at generation 1")

	// A later confirm-window generation is provisionally applied, then reverted.
	applier.provisionalGen = 2
	applier.revertCfg = boot
	applier.requireRevert(ctx, 2, "local confirm-window deadman")
	require.Equal(t, uint64(2), applier.revertedGen, "precondition: the revert completed")

	assert.Equal(t, uint64(1), applier.terminalGen,
		"a completed revert must not retract the artifact latch it did nothing about")
	assert.Equal(t, uint64(1), applier.obs.status().TerminalGeneration,
		"the operator-facing signal must still name the member that cannot boot correctly")
}

// TestClusterRolloutRepair_ANewerAppliedGenerationRetractsTheRevertLatch is the
// other side of the same rule. A revert latch says this member is still running
// config the cohort REJECTED; once it is running a NEWER generation the cohort
// decided, that is no longer true and the latch has to go — a terminal signal
// that outlives its condition sends an operator to replace a healthy member.
func TestClusterRolloutRepair_ANewerAppliedGenerationRetractsTheRevertLatch(t *testing.T) {
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	host := newFakeRolloutHost(soloCohortConfig(0))
	applier := &rolloutApplier{
		host: host, memberID: "node-a", clk: fake,
		obs: newRolloutObserver(nil, fake, time.Second, "node-a"),
	}
	applier.markUnsafe(2, rolloutRepairRevert,
		"this member is still running the REJECTED provisional config")

	rollForward := soloCohortConfig(11)
	rollForward.Bindings[0].Address = "addr/rolled-forward"
	require.True(t, applier.applyCommittedGeneration(context.Background(), 3, 11, rollForward, rollForward))

	assert.Zero(t, applier.terminalGen,
		"a member now running a newer cohort-decided generation is not running the rejected one")
	assert.Zero(t, applier.obs.status().TerminalGeneration)
}

// TestClusterRolloutRepair_ACompletedRevertResolvesAFailedApply pins the other
// crossing case. The apply latch says this member could not reach a generation;
// once its own deadman has reverted that generation, the cohort has abandoned it
// and there is nothing left to reach. Leaving the latch standing sends an
// operator to replace a member that is correctly running its safe config.
func TestClusterRolloutRepair_ACompletedRevertResolvesAFailedApply(t *testing.T) {
	ctx := context.Background()
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	boot := soloCohortConfig(0)
	host := newFakeRolloutHost(boot)
	applier := &rolloutApplier{
		host: host, memberID: "node-a", clk: fake,
		obs: newRolloutObserver(nil, fake, time.Second, "node-a"),
	}
	applier.markUnsafe(2, rolloutRepairApply, "could not apply generation 2 on this member")

	// The member never swapped, so it is already on the revert target and the
	// repair completes on its first attempt.
	applier.provisionalGen = 2
	applier.revertCfg = boot
	applier.requireRevert(ctx, 2, "local confirm-window deadman")
	require.Equal(t, uint64(2), applier.revertedGen, "precondition: the revert completed")

	assert.Zero(t, applier.terminalGen,
		"a generation the deadman abandoned is not one this member still has to reach")
}

// TestClusterRolloutConfirm_ResolvesAFailedRevertAndRecordsTheArtifact covers
// the crossing case: a member whose revert failed to the bound never left the
// provisional generation — and then the cohort CONFIRMS that generation.
//
// Two things have to happen, and neither did. The member is now running exactly
// the confirmed config, so the latch reading "still running the REJECTED config,
// replace this member" is no longer true and must go. And it owes the durable
// artifact for the generation it is running: without it a restart boots this
// member older than the cohort, which is the residual the artifact exists to
// close — silently skipped because the guard that suppresses a hopeless artifact
// retry did not check WHICH repair had gone terminal.
func TestClusterRolloutConfirm_ResolvesAFailedRevertAndRecordsTheArtifact(t *testing.T) {
	ctx := context.Background()
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	store := memoryrollout.NewStore(memoryrollout.WithClock(fake))
	codec := newConfigCodecFake()

	candidate := soloWindowConfig(7, time.Second)
	candidate.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(candidate)
	require.True(t, ok)
	codec.register(candidate)

	// The member is already running the provisional generation and could not
	// revert off it: exactly the state driveRevert leaves behind at its bound.
	host := newFakeRolloutHost(candidate)
	barrier := &rolloutBarrier{
		store: store, committedStore: store, memberID: "node-a",
		pollInterval: time.Second, ops: newRolloutOps(0),
		encode: codec.encode, decode: codec.decode,
	}
	barrier.stage(digest, candidate, candidate)
	applier := &rolloutApplier{
		host: host, barrier: barrier, store: store, memberID: "node-a",
		clk: fake, obs: newRolloutObserver(nil, fake, time.Second, "node-a"),
	}
	applier.provisionalGen = 1
	applier.markUnsafe(1, rolloutRepairRevert,
		"this member is still running the REJECTED provisional config — replace this member")

	seedWindowedCommit(t, store, digest, 7, time.Second)
	confirmRollout(t, store, 1)
	require.NoError(t, applier.tick(ctx))

	assert.Zero(t, applier.terminalGen,
		"the cohort confirmed the generation this member is running; it is not running a rejected config")
	got, err := store.CommittedConfig(ctx)
	require.NoError(t, err, "the confirmed generation must be recorded durably, terminal latch or not")
	assert.Equal(t, uint64(1), got.Generation, "a restart must boot on the generation the cohort confirmed")
}

// TestClusterRolloutConfirm_ResolvesAFailedRevertWithoutACommittedArtifactStore
// is the same crossing case on a deployment that wires no config codec, which is
// a supported shape (the durable-artifact residual protection is simply off).
// There the reconcile fallback cannot run at all, so the confirmed generation's
// own path is the ONLY thing that can retract the latch — and it does that by
// going through the apply, whose first branch is the already-running check.
func TestClusterRolloutConfirm_ResolvesAFailedRevertWithoutACommittedArtifactStore(t *testing.T) {
	ctx := context.Background()
	fake := clocktest.NewAt(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	store := memoryrollout.NewStore(memoryrollout.WithClock(fake))

	candidate := soloWindowConfig(7, time.Second)
	candidate.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(candidate)
	require.True(t, ok)

	host := newFakeRolloutHost(candidate) // already running the provisional gen
	barrier := &rolloutBarrier{store: store, memberID: "node-a", pollInterval: time.Second, ops: newRolloutOps(0)}
	barrier.stage(digest, candidate, candidate)
	applier := &rolloutApplier{
		host: host, barrier: barrier, store: store, memberID: "node-a",
		clk: fake, obs: newRolloutObserver(nil, fake, time.Second, "node-a"),
	}
	applier.provisionalGen = 1
	applier.markUnsafe(1, rolloutRepairRevert,
		"this member is still running the REJECTED provisional config — replace this member")

	seedWindowedCommit(t, store, digest, 7, time.Second)
	confirmRollout(t, store, 1)
	require.NoError(t, applier.tick(ctx))

	assert.Zero(t, applier.terminalGen,
		"the confirmed generation's own path must retract the latch; nothing else can here")
	assert.Zero(t, applier.obs.status().TerminalGeneration)
}

// seedCommit proposes and commits digest as the next generation for a solo
// cohort, standing in for the elected coordinator.
func seedCommit(t *testing.T, store *memoryrollout.Store, digest string, version int) {
	t.Helper()
	seedCommitWindow(t, store, digest, version, 0)
}

// seedWindowedCommit is seedCommit for a confirm-window rollout: the commit is
// provisional and expires after window.
func seedWindowedCommit(t *testing.T, store *memoryrollout.Store, digest string, version int, window time.Duration) {
	t.Helper()
	seedCommitWindow(t, store, digest, version, window)
}

func seedCommitWindow(t *testing.T, store *memoryrollout.Store, digest string, version int, window time.Duration) {
	t.Helper()
	ctx := context.Background()
	r, err := store.Propose(ctx, persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: version,
		Members: []string{"node-a"}, TTL: time.Hour, ConfirmWindow: window,
	})
	require.NoError(t, err)
	require.NoError(t, store.Ack(ctx, r.Generation(), "node-a", digest))
	require.NoError(t, store.Commit(ctx, r.Generation(),
		persistence.LeaseToken{Owner: "coord", Version: 1}))
}

// confirmRollout stands in for the elected coordinator confirming a
// provisionally-committed generation whose members all converged.
func confirmRollout(t *testing.T, store *memoryrollout.Store, gen uint64) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.Converge(ctx, gen, "node-a"))
	require.NoError(t, store.Confirm(ctx, gen, persistence.LeaseToken{Owner: "coord", Version: 1}))
}

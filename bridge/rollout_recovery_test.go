package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Retryable-safety-state tests for the coordinated cluster rollout drive.
//
// Two operations decide whether a member is SAFE after the cohort has already
// decided, and both used to be one-shot and best-effort:
//
//   - recording the durable last-committed artifact. Fail it, and this member
//     reboots onto an older generation than the cohort is running.
//   - reverting a provisional generation to the last confirmed one. Fail it, and
//     this member keeps running a config the cohort rejected.
//
// Neither may be latched as done before it is VERIFIED, both must be retried
// under a bounded backoff, and a member that cannot reach its safe generation
// must say so loudly rather than pretend it succeeded.

// TestClusterRolloutArtifact_RetriesUntilTheWriteIsVerified pins the retry. The
// artifact write ran on the apply context and was never retried, so a single
// cancelled PutItem left the cohort with a committed generation and no durable
// artifact to boot on — observed in an integration run, not theorised.
func TestClusterRolloutArtifact_RetriesUntilTheWriteIsVerified(t *testing.T) {
	store := newArtifactFaultStore(2, false) // the first two writes fail outright
	boot := soloCohortConfig(0)
	host := newFakeRolloutHost(boot)

	rc := fastRolloutConfig(store, "node-a")
	rc.Encode = newConfigCodecFake().encode
	d := NewClusterRolloutDriver(host, rc)
	require.NotNil(t, d)

	stop := d.Start(context.Background(), clock.System, nil)
	defer stop(context.Background())

	candidate := soloCohortConfig(7)
	candidate.Bindings[0].Address = "addr/rolled"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))

	wait.Until(t, 10*time.Second, "the retried artifact write finally lands", func() bool {
		cfg, err := store.CommittedConfig(context.Background())
		return err == nil && cfg.Generation == 1
	})

	st, ok := d.Status()
	require.True(t, ok)
	assert.Equal(t, uint64(1), st.ArtifactGeneration,
		"the member reports the artifact recorded only for a generation it verified")
	assert.Zero(t, st.TerminalGeneration, "a write that recovered is not a terminal condition")
	assert.GreaterOrEqual(t, store.writeCount(), 3, "the two failures must have been retried")
}

// TestClusterRolloutArtifact_ALyingWriteIsNeverLatched is the "no completion
// latch precedes verification" half. This store REPORTS every write durable and
// persists nothing — the shape a conditional write degrades into when the
// caller's assumption about it is wrong, and the shape no error check can catch.
// The member must read the artifact back, refuse to latch what it cannot verify,
// and — once the bound is reached — say so loudly instead of silently promising a
// boot state it does not have.
func TestClusterRolloutArtifact_ALyingWriteIsNeverLatched(t *testing.T) {
	store := newArtifactFaultStore(0, true) // reports success, writes nothing
	boot := soloCohortConfig(0)
	host := newFakeRolloutHost(boot)

	rc := fastRolloutConfig(store, "node-a")
	rc.Encode = newConfigCodecFake().encode
	d := NewClusterRolloutDriver(host, rc)
	require.NotNil(t, d)

	stop := d.Start(context.Background(), clock.System, nil)
	defer stop(context.Background())

	candidate := soloCohortConfig(7)
	candidate.Bindings[0].Address = "addr/rolled"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))

	wait.Until(t, 10*time.Second, "the member declares the unverifiable artifact terminal", func() bool {
		st, ok := d.Status()
		return ok && st.TerminalGeneration == 1
	})

	st, _ := d.Status()
	assert.Zero(t, st.ArtifactGeneration, "an unverifiable write must never latch as recorded")
	assert.NotEmpty(t, host.degradedReason(), "the member must be visibly degraded, not quietly wrong")

	// It keeps trying, because this member is running the RIGHT config and only
	// its boot state is wrong: replacing it is the one action that would make
	// things worse, and the write is what fixes it if the store comes back. What
	// must be bounded is the RATE. The backoff doubles each attempt, so the gap
	// between writes outgrows this window even though the drive keeps ticking
	// every 5 ms — a retry-every-tick loop could never satisfy it.
	wait.StableFor(t, store.writeCount, 200*time.Millisecond, 10*time.Second)
}

// windowedRevertDrive wires a solo cohort whose candidate never converges, so the
// confirm window always ends in a revert to the last confirmed generation, and
// makes the host refuse the revert `refusals` times (-1 = forever).
func windowedRevertDrive(
	t *testing.T, refusals int,
) (*ClusterRolloutDriver, *fakeRolloutHost) {
	t.Helper()
	// Long enough that the provisional swap always lands inside the window, so the
	// revert under test is the one the EXPIRY triggers rather than an artefact of a
	// slow machine missing the window entirely.
	const window = time.Second
	boot := soloWindowConfig(0, window)
	boot.Bindings[0].Address = "addr/original"
	host := newFakeRolloutHost(boot)
	host.unconverged[7] = true
	host.refuse[0] = refusals // refuse to swap BACK to the last confirmed generation

	store := newArtifactFaultStore(0, false)
	rc := fastRolloutConfig(store, "node-a")
	rc.Encode = newConfigCodecFake().encode
	d := NewClusterRolloutDriver(host, rc)
	require.NotNil(t, d)

	stop := d.Start(context.Background(), clock.System, nil)
	t.Cleanup(func() { stop(context.Background()) })

	candidate := soloWindowConfig(7, window)
	candidate.Bindings[0].Address = "addr/never-converges"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))
	wait.Until(t, 5*time.Second, "the member provisionally swaps to the candidate", func() bool {
		return host.Config().Version == 7
	})
	return d, host
}

// TestClusterRolloutRevert_RetriesUntilTheMemberRunsTheSafeGeneration pins the
// revert retry. A revert was one-shot: the member marked the generation reverted
// whether or not the swap back took, so a transient failure (a broker refusing
// the reconnect, a store briefly unopenable) left it running the REJECTED
// provisional config with its own repair latched off.
func TestClusterRolloutRevert_RetriesUntilTheMemberRunsTheSafeGeneration(t *testing.T) {
	d, host := windowedRevertDrive(t, 2) // the first two revert attempts fail

	wait.Until(t, 15*time.Second, "the retried revert lands on the last confirmed generation", func() bool {
		return host.Config().Version == 0
	})
	assert.Equal(t, "addr/original", host.Config().Bindings[0].Address)

	st, ok := d.Status()
	require.True(t, ok)
	assert.Zero(t, st.TerminalGeneration, "a revert that recovered is not a terminal condition")
}

// TestClusterRolloutRevert_TerminalWhenTheSafeGenerationCannotBeReached is the
// terminal fallback. A member that cannot get back to the last confirmed
// generation is running config the cohort rejected and cannot repair itself: it
// must stop retrying, latch a terminal generation and mark itself degraded so
// the deployment replaces it, rather than looping on a swap that will not take.
func TestClusterRolloutRevert_TerminalWhenTheSafeGenerationCannotBeReached(t *testing.T) {
	d, host := windowedRevertDrive(t, -1) // every revert attempt fails

	wait.Until(t, 15*time.Second, "the member declares the unreachable safe generation terminal", func() bool {
		st, ok := d.Status()
		return ok && st.TerminalGeneration == 1
	})
	assert.Equal(t, 7, host.Config().Version,
		"the member is still on the rejected provisional config — that is why it is terminal")
	assert.NotEmpty(t, host.degradedReason())

	// Bounded: it stops trying to swap rather than rebuilding the runtime forever.
	wait.StableFor(t, host.appliedCount, 200*time.Millisecond, 5*time.Second)
}

// TestClusterRolloutRevert_IsDroppedWhenANewerGenerationIsApplied pins the
// supersession rule. A revert that is still owed when this member applies a
// NEWER generation is no longer a repair — its target is two generations back —
// and leaving it armed would make the drive fight itself: the repair swapping the
// runtime to N-1 on one tick and the newer generation's adopt swapping it back on
// the next, forever.
func TestClusterRolloutRevert_IsDroppedWhenANewerGenerationIsApplied(t *testing.T) {
	running := soloCohortConfig(9)
	host := newFakeRolloutHost(running)
	applier := &rolloutApplier{host: host, memberID: "node-a", clk: clock.System}
	applier.revertCfg = soloCohortConfig(0)
	applier.pendingRevert = &rolloutRepair{gen: 1, cfg: applier.revertCfg}
	applier.gate.record(2) // a newer generation has since been applied

	applier.driveRevert(context.Background())

	assert.Nil(t, applier.pendingRevert, "a superseded revert must be dropped, not retried")
	assert.Zero(t, host.appliedCount(), "and it must not swap the runtime on its way out")
	assert.Equal(t, 9, host.Config().Version)
}

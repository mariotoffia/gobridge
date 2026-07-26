package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Whole-barrier tests: one process running BOTH halves of the drive (applier and
// coordinator) against a real rollout store, with nothing hand-driven. They are
// the proof that the pieces compose — every other rollout test isolates one half
// and stands in for the other, which cannot catch a wiring gap between them.
//
// The cohort is a single member so this process's own ack satisfies the epoch.
// That is a legitimate cohort shape, not a shortcut: invariant I2 is "acks cover
// the epoch", and a one-member epoch exercises the same code path a three-member
// one does. Multi-PROCESS coverage is Phase 5's UC-CR1 on the nodeProcess
// harness; it cannot be reproduced in-process because each member needs its own
// config source.

// soloCohortConfig is a coordinated clustered config whose roster is this node
// alone, so the barrier resolves without a peer.
func soloCohortConfig(version int) *ports.BridgeConfig {
	cfg := clusteredCfg("r1")
	cfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"node-a"}}
	cfg.Version = version
	return cfg
}

// runSoloCohort starts a Supervisor wired as the only member of a coordinated
// cohort, with a cadence short enough that the barrier resolves promptly. The
// lease TTL doubles as the coordinator's lock delay, so it bounds how long the
// first decision takes.
func runSoloCohort(t *testing.T, rec *ports.RecordingExporter) (*Supervisor, chan *ports.BridgeConfig, <-chan SwapEvent) {
	t.Helper()
	store := memoryrollout.NewStore()
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = 5 * time.Millisecond
	rc.LeaseTTL = 20 * time.Millisecond
	onSwap, swaps := swapChan(4)

	opts := []SupervisorOption{WithOnSwap(onSwap), WithClusterRollout(rc)}
	if rec != nil {
		opts = append(opts, WithSupervisorMetrics(rec))
	}
	s := newTestSupervisor(opts...)

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, soloCohortConfig(0), changes)
	t.Cleanup(func() { cancel(); <-errCh })
	return s, changes, swaps
}

// rolloutState reads the barrier's state through the Supervisor's own published
// status, i.e. the same projection an operator reads on /deephealth.
func rolloutState(s *Supervisor) string {
	st, ok := s.RolloutStatus()
	if !ok {
		return ""
	}
	return st.State
}

// TestClusterRollout_EndToEnd_HappyPath is the §10 single-process happy path:
// propose -> ack -> commit -> swap, driven end to end by the wired barrier with
// no step hand-executed. It is the test that would fail if the applier, the
// coordinator, or the wiring between them regressed.
func TestClusterRollout_EndToEnd_HappyPath(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s, changes, swaps := runSoloCohort(t, rec)
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	candidate := soloCohortConfig(99)
	candidate.Bindings[0].Address = "addr/rolled"
	require.True(t, sendConfig(changes, candidate, time.Second))

	// The reload is DEFERRED to the barrier, not applied inline.
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)
	require.True(t, ev.Deferred, "a coordinated delta must never apply inline")
	require.Equal(t, DeferReasonRolloutPending, ev.DeferReason)
	assert.Same(t, oldRt, s.Runtime(), "nothing may swap before the barrier commits")

	// ...and then the barrier itself carries it to a local swap.
	wait.Until(t, 5*time.Second, "the barrier commits and this member applies it", func() bool {
		return s.Config().Version == 99
	})

	assert.Equal(t, "addr/rolled", s.Config().Bindings[0].Address)
	assert.NotSame(t, oldRt, s.Runtime(), "the committed generation really swapped")
	assert.Equal(t, string(persistence.RolloutCommitted), rolloutState(s))
	assert.Equal(t, 1, countEntries(rec, shared.MetricClusterRolloutResolved, shared.TagKeyOutcome, "committed"),
		"the resolution must be observable exactly once")
}

// TestClusterRollout_EndToEnd_AbortLeavesTheRunningConfigServing is the §10
// abort path. A member that cannot build the candidate nacks; the coordinator
// aborts; NOTHING swaps and the old config keeps serving. This is the property
// that makes the barrier safe to enable: a bad config costs an abort, not an
// outage.
func TestClusterRollout_EndToEnd_AbortLeavesTheRunningConfigServing(t *testing.T) {
	rec := &ports.RecordingExporter{}
	store := memoryrollout.NewStore()
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = 5 * time.Millisecond
	rc.LeaseTTL = 20 * time.Millisecond
	onSwap, _ := swapChan(4)
	s := newTestSupervisor(WithSupervisorMetrics(rec), WithOnSwap(onSwap), WithClusterRollout(rc))

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, soloCohortConfig(0), changes)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	oldAddr := s.Config().Bindings[0].Address

	// A peer proposes a candidate this member cannot build (a route referencing a
	// receiver that does not exist), and stages it as if its own source had
	// delivered it. The applier must nack rather than ack-and-hope.
	unbuildable := soloCohortConfig(99)
	unbuildable.Routes[0].ReceiverID = "no-such-receiver"
	digest, ok := configCanonicalBytesDigest(unbuildable)
	require.True(t, ok)
	s.rollout.stage(digest, unbuildable, unbuildable)
	_, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: 99,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)

	wait.Until(t, 5*time.Second, "the barrier aborts on the nack", func() bool {
		return rolloutState(s) == string(persistence.RolloutAborted)
	})

	assert.Equal(t, 0, s.Config().Version, "an aborted rollout must not advance the applied config")
	assert.Equal(t, oldAddr, s.Config().Bindings[0].Address)
	assert.Same(t, oldRt, s.Runtime(), "the running runtime keeps serving")
	assert.Equal(t, 1, countEntries(rec, shared.MetricClusterRolloutResolved, shared.TagKeyOutcome, "aborted"))

	st, ok := s.RolloutStatus()
	require.True(t, ok)
	assert.Contains(t, st.Nacked, "node-a")
	assert.NotEmpty(t, st.Reason, "the abort must carry the reason an operator needs")
}

// TestClusterRollout_EndToEnd_SecondChangeRollsAfterTheFirst proves the barrier
// is reusable: generations are monotonic and a committed rollout does not block
// the next one. A one-shot barrier would look identical in every single-rollout
// test and fail the first time an operator made two changes.
func TestClusterRollout_EndToEnd_SecondChangeRollsAfterTheFirst(t *testing.T) {
	s, changes, swaps := runSoloCohort(t, nil)

	// Only the FIRST swap event is asserted directly. After a barrier commit the
	// applier's own swap fires onSwap too, so the channel interleaves deferrals
	// and applications; the applied config version is the unambiguous signal.
	first := soloCohortConfig(1)
	first.Bindings[0].Address = "addr/first"
	require.True(t, sendConfig(changes, first, time.Second))
	require.True(t, awaitSwap(t, swaps).Deferred, "the first change defers to the barrier")
	wait.Until(t, 5*time.Second, "the first change commits", func() bool {
		return s.Config().Version == 1
	})

	second := soloCohortConfig(2)
	second.Bindings[0].Address = "addr/second"
	require.True(t, sendConfig(changes, second, time.Second))
	wait.Until(t, 5*time.Second, "the second change commits", func() bool {
		return s.Config().Version == 2
	})

	assert.Equal(t, "addr/second", s.Config().Bindings[0].Address)
	st, ok := s.RolloutStatus()
	require.True(t, ok)
	assert.Equal(t, uint64(2), st.Generation, "generations advance; the barrier is not one-shot")
}

// TestClusterRollout_EndToEnd_ReplacementRequiredDeltaIsRefused proves the §8
// class gate survives the guard lift. A delta that changes the cohort's OWN
// roster must still be refused OUTRIGHT — not proposed and aborted, but never
// proposed at all, keeping ADR 0012's whole-cohort replacement contract intact.
// The roster is the epoch the barrier freezes, so it is the one change the
// barrier structurally cannot carry.
func TestClusterRollout_EndToEnd_ReplacementRequiredDeltaIsRefused(t *testing.T) {
	s, changes, swaps := runSoloCohort(t, nil)

	destructive := soloCohortConfig(99)
	destructive.Bridge.Cluster.Members = []string{"node-a", "node-b"}
	require.True(t, sendConfig(changes, destructive, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error, "a replacement-required delta is refused, not coordinated")
	assert.False(t, ev.Deferred)
	assert.Contains(t, ev.Error.Error(), "replacement-required")

	_, ok := s.RolloutStatus()
	require.True(t, ok)
	st, _ := s.RolloutStatus()
	assert.Empty(t, st.State, "no rollout may be opened for a replacement-required delta")
	assert.Equal(t, 0, s.Config().Version)
}

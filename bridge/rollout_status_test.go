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

// gaugeValue returns the last recorded value of a gauge series with the given
// tag value, or false when the series was never emitted.
func gaugeValue(rec *ports.RecordingExporter, name, tagKey, tagValue string) (float64, bool) {
	var out float64
	var found bool
	for _, e := range rec.FindEntries(name) {
		if tagKey == "" {
			out, found = e.FValue, true
			continue
		}
		for _, tag := range e.Tags {
			if tag.Key == tagKey && tag.Value == tagValue {
				out, found = e.FValue, true
			}
		}
	}
	return out, found
}

// countEntries returns how many entries of a series carry the given tag.
func countEntries(rec *ports.RecordingExporter, name, tagKey, tagValue string) int {
	n := 0
	for _, e := range rec.FindEntries(name) {
		for _, tag := range e.Tags {
			if tag.Key == tagKey && tag.Value == tagValue {
				n++
			}
		}
	}
	return n
}

// TestRolloutObserver_PublishesStateAsAOneHotGauge proves the state gauge zeroes
// every OTHER state, not just sets the current one. A gauge that only ever set
// the live state would leave the previous state latched at 1 forever in any
// pull-based exporter — an operator alerting on "staging == 1" would page
// permanently after the first rollout ever committed.
func TestRolloutObserver_PublishesStateAsAOneHotGauge(t *testing.T) {
	rec := &ports.RecordingExporter{}
	obs := &rolloutObserver{metrics: rec}
	store := memoryrollout.NewStore()
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "d", ConfigVersion: 3,
		Members: []string{"node-a", "node-b"}, TTL: time.Hour,
	})
	require.NoError(t, err)

	obs.observe(r, "node-a", true, false)

	for _, tc := range []struct {
		state string
		want  float64
	}{
		{"proposed", 1},
		{"staging", 0},
		{"committed", 0},
		{"aborted", 0},
	} {
		got, found := gaugeValue(rec, shared.MetricClusterRolloutState, shared.TagKeyState, tc.state)
		require.True(t, found, "state %q must be emitted so it can be zeroed", tc.state)
		assert.Equal(t, tc.want, got, "state %q", tc.state)
	}

	acks, found := gaugeValue(rec, shared.MetricClusterRolloutAcks, "", "")
	require.True(t, found)
	assert.Equal(t, 0.0, acks)
	epoch, found := gaugeValue(rec, shared.MetricClusterRolloutEpoch, "", "")
	require.True(t, found)
	assert.Equal(t, 2.0, epoch, "the epoch size is what acks must reach for the barrier to open")
}

// TestRolloutObserver_CountsEachResolutionOnce proves the resolution counter
// fires per ROLLOUT, not per observation. The drive polls the row every couple
// of seconds, so a counter that fired on every poll of an already-decided row
// would turn one committed rollout into an unbounded, meaningless rate.
func TestRolloutObserver_CountsEachResolutionOnce(t *testing.T) {
	rec := &ports.RecordingExporter{}
	obs := &rolloutObserver{metrics: rec}
	store := memoryrollout.NewStore()
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "d", ConfigVersion: 3,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	require.NoError(t, store.Ack(context.Background(), r.Generation(), "node-a", "build"))
	require.NoError(t, store.Commit(context.Background(), r.Generation(),
		persistence.LeaseToken{Owner: "coord", Version: 1}))
	committed, err := store.Current(context.Background())
	require.NoError(t, err)

	for range 5 {
		obs.observe(committed, "node-a", true, true)
	}

	assert.Equal(t, 1, countEntries(rec, shared.MetricClusterRolloutResolved, shared.TagKeyOutcome, "committed"),
		"one rollout resolves exactly once, however often it is observed")
	assert.Equal(t, 0, countEntries(rec, shared.MetricClusterRolloutResolved, shared.TagKeyOutcome, "aborted"))
}

// TestRolloutObserver_CountsAnAbortedResolution proves the abort outcome is
// counted under its own tag, so a cohort whose changes keep being rejected is
// distinguishable from one whose changes keep succeeding.
func TestRolloutObserver_CountsAnAbortedResolution(t *testing.T) {
	rec := &ports.RecordingExporter{}
	obs := &rolloutObserver{metrics: rec}
	store := memoryrollout.NewStore()
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "d", ConfigVersion: 3,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	require.NoError(t, store.Abort(context.Background(), r.Generation(),
		persistence.LeaseToken{Owner: "coord", Version: 1}, "a peer nacked"))
	aborted, err := store.Current(context.Background())
	require.NoError(t, err)

	obs.observe(aborted, "node-a", false, false)

	assert.Equal(t, 1, countEntries(rec, shared.MetricClusterRolloutResolved, shared.TagKeyOutcome, "aborted"))
}

// TestRolloutObserver_SnapshotNamesWhoTheCohortWaitsFor proves the deep-health
// projection answers the only question that matters during a stuck rollout: the
// epoch, who voted, and whether THIS member even has the candidate yet. Without
// the staged flag, a member whose config source is lagging looks identical to one
// that is merely slow to build.
func TestRolloutObserver_SnapshotNamesWhoTheCohortWaitsFor(t *testing.T) {
	obs := &rolloutObserver{}
	store := memoryrollout.NewStore()
	r, err := store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: "d", ConfigVersion: 42,
		Members: []string{"node-c", "node-a", "node-b"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	require.NoError(t, store.Ack(context.Background(), r.Generation(), "node-a", "build"))
	require.NoError(t, store.Nack(context.Background(), r.Generation(), "node-b", "cannot build"))
	staged, err := store.Current(context.Background())
	require.NoError(t, err)

	obs.observe(staged, "node-a", false, false)
	got := obs.status()

	assert.Equal(t, "node-a", got.MemberID)
	assert.Equal(t, 42, got.ConfigVersion)
	assert.Equal(t, string(persistence.RolloutStaging), got.State)
	assert.Equal(t, []string{"node-a", "node-b", "node-c"}, got.Epoch, "epoch is sorted, not map-ordered")
	assert.Equal(t, []string{"node-a"}, got.Acked)
	assert.Equal(t, []string{"node-b"}, got.Nacked)
	assert.False(t, got.Staged, "this member has not received the candidate from its own config source")
}

// TestSupervisorRolloutStatus_AbsentWithoutABarrier proves a deployment that
// runs no barrier reports no rollout section at all, rather than an empty one an
// operator could misread as "a rollout exists and is stuck".
func TestSupervisorRolloutStatus_AbsentWithoutABarrier(t *testing.T) {
	s := newTestSupervisor()
	_, ok := s.RolloutStatus()
	assert.False(t, ok)
}

// TestSupervisorRolloutStatus_PublishedByTheDrive proves the status an operator
// reads is produced by the running drive — the end-to-end wiring from
// Supervisor.Run through the applier's observation to RolloutStatus.
func TestSupervisorRolloutStatus_PublishedByTheDrive(t *testing.T) {
	rec := &ports.RecordingExporter{}
	store := memoryrollout.NewStore()
	onSwap, swaps := swapChan(4)
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = 5 * time.Millisecond
	rc.LeaseTTL = time.Hour
	s := newTestSupervisor(WithSupervisorMetrics(rec), WithOnSwap(onSwap), WithClusterRollout(rc))

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), changes)
	defer func() { cancel(); <-errCh }()

	require.True(t, sendConfig(changes, liveSafeCandidate(99), time.Second))
	require.True(t, awaitSwap(t, swaps).Deferred)

	wait.Until(t, 2*time.Second, "the drive publishes the barrier state", func() bool {
		st, ok := s.RolloutStatus()
		return ok && st.State != "" && len(st.Acked) == 1
	})

	st, ok := s.RolloutStatus()
	require.True(t, ok)
	assert.Equal(t, "node-a", st.MemberID)
	assert.Equal(t, 99, st.ConfigVersion)
	assert.Equal(t, []string{"node-a", "node-b"}, st.Epoch)
	assert.Equal(t, []string{"node-a"}, st.Acked)
	assert.True(t, st.Staged, "the proposer stages the candidate it proposed")

	value, found := gaugeValue(rec, shared.MetricClusterRolloutState, shared.TagKeyState, "staging")
	require.True(t, found)
	assert.Equal(t, 1.0, value, "the barrier state must be visible as a metric, not only in health")
}

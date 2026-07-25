package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// coordinatedClusteredCfg is clusteredCfg opted into cluster.rollout:
// coordinated — the steady state in which live-safe deltas are eligible for the
// coordinated rollout barrier instead of the ADR 0012 refusal.
// coordinatedClusteredCfg builds a clustered config opted into the coordinated
// rollout barrier, with the two-member roster the barrier tests freeze as their
// membership epoch. The roster lives in bridge.cluster.members — NOT
// cluster.endpoints, which is this instance's capability map and would freeze a
// one-"member" epoch named "http".
func coordinatedClusteredCfg(id string) *ports.BridgeConfig {
	cfg := clusteredCfg(id)
	cfg.Bridge.Cluster = &ports.ClusterConfig{
		Rollout: "coordinated",
		Members: []string{"node-a", "node-b"},
	}
	return cfg
}

// TestSupervisorCoordinatedRollout_LiveSafeDeltaFailsClosedUntilBarrierWired
// proves the Phase-3 guard lift is SAFE while the barrier drive is unwired: a
// live-safe delta in a coordinated cluster is recognized as coordinated-eligible
// (distinct from the generic clustered refusal) but, because the proposer/
// applier/coordinator goroutines are a later phase, it fails CLOSED — a visible
// error, the running config kept, and crucially NOT a success-shaped deferral
// that would silently drop the change while acking it as applied. When the
// barrier lands, this branch drives it instead of erroring.
func TestSupervisorCoordinatedRollout_LiveSafeDeltaFailsClosedUntilBarrierWired(t *testing.T) {
	rec := &ports.RecordingExporter{}
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithSupervisorMetrics(rec), WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)
	oldCfg := s.Config()
	require.NotNil(t, oldCfg)

	// A live-safe delta (version-only bump) within the coordinated cluster.
	proposed := coordinatedClusteredCfg("r1")
	proposed.Version = 99
	require.True(t, sendConfig(ch, proposed, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error, "the barrier is unwired; a coordinated live-safe delta must fail closed, not silently defer")
	assert.False(t, ev.Deferred, "must NOT be a success-shaped deferral (that would drop the change while acking it)")
	assert.Contains(t, ev.Error.Error(), "coordinated", "the error must name the coordinated rollout path it is eligible for")

	// The delta is NOT applied: the running runtime, applied config, and version
	// are all unchanged, and the failure-tagged reload counter is exported.
	assert.Same(t, oldRt, s.Runtime(), "the running runtime instance must be untouched")
	assert.True(t, oldRt.IsRunning(), "the current runtime must keep serving")
	assert.Equal(t, 0, s.Config().Version, "the applied config version must not advance")
	assert.True(t, counterHasState(rec.FindEntries(shared.MetricConfigReloads), "failure"),
		"a fail-closed coordinated refusal must export the failure-tagged counter")
}

// TestSupervisorCoordinatedRollout_ReplacementRequiredStillRefused proves the
// lift does not weaken safety: a delta that is not a live-safe change within the
// coordinated cluster (here leaving the cohort) is still refused fail-closed and
// never deferred. The class-reason wording for a within-cohort
// replacement-required delta is covered by the classifyClusterReload unit test.
func TestSupervisorCoordinatedRollout_ReplacementRequiredStillRefused(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, coordinatedClusteredCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	// Leaving the coordinated cluster (deployment-mode change) is
	// replacement-required and must keep failing closed.
	proposed := coordinatedClusteredCfg("r1")
	proposed.Version = 7
	proposed.Bridge.DeploymentMode = "standalone"
	proposed.Bridge.Cluster = nil
	require.True(t, sendConfig(ch, proposed, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error, "a replacement-required delta must fail closed even in coordinated mode")
	assert.False(t, ev.Deferred, "a refusal is a definitive failure, not a deferral")
	assert.Same(t, oldRt, s.Runtime(), "the running runtime instance must be untouched")
}

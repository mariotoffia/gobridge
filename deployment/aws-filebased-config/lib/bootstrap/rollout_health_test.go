package bootstrap

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// The deep-health projection of the coordinated cluster rollout.
//
// The barrier's guarantee is atomic BEFORE the commit and per-member AFTER it
// (ADR 0013), so the window in which one member runs an older generation than
// its peers is a NORMAL part of the protocol — and the only thing that keeps it
// from becoming an unnoticed permanent split is this projection. Two rules
// therefore matter more than the field list: a member that has not applied a
// decided generation is degraded, and a member whose observation has gone stale
// is degraded even when everything it last saw looked fine.

// TestRolloutHealth_MapsEveryDivergenceField pins the mapping. Every field the
// barrier publishes exists because some divergence is invisible without it; a
// mapping that silently drops one leaves the operator reading a complete-looking
// block with the answer missing.
func TestRolloutHealth_MapsEveryDivergenceField(t *testing.T) {
	observedAt := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	status := bridge.RolloutStatus{
		MemberID:           "node-a",
		Generation:         4,
		State:              "committed",
		ConfigVersion:      12,
		Epoch:              []string{"node-a", "node-b"},
		Acked:              []string{"node-a", "node-b"},
		Nacked:             nil,
		Converged:          []string{"node-b"},
		Reason:             "",
		Staged:             true,
		Applied:            false,
		ConfirmPending:     false,
		ObservedAt:         observedAt,
		ObservationAge:     9 * time.Second,
		Stale:              true,
		LastError:          "rollout store read timed out",
		ArtifactGeneration: 3,
		TerminalGeneration: 4,
		TerminalReason:     "replace this member",
	}

	got := rolloutHealth(status, &rolloutBaseline{Generation: 1, Digest: "abc"})

	require.NotNil(t, got)
	assert.Equal(t, "node-a", got.MemberID)
	assert.Equal(t, uint64(4), got.Generation)
	assert.Equal(t, "committed", got.State)
	assert.False(t, got.ConfirmPending, "a final commit is not a provisional one")
	assert.Equal(t, 12, got.ConfigVersion)
	assert.Equal(t, []string{"node-a", "node-b"}, got.Epoch)
	assert.Equal(t, []string{"node-a", "node-b"}, got.Acked)
	assert.Equal(t, []string{"node-b"}, got.Converged,
		"epoch minus converged is who the confirm barrier is waiting for")
	assert.True(t, got.CandidateStaged)
	assert.False(t, got.Applied)
	assert.Equal(t, observedAt, got.ObservedAt)
	assert.Equal(t, int64(9000), got.ObservationAgeMS)
	assert.True(t, got.Stale)
	assert.Equal(t, "rollout store read timed out", got.LastError)
	assert.Equal(t, uint64(3), got.ArtifactGeneration)
	assert.Equal(t, uint64(4), got.TerminalGeneration)
	assert.Equal(t, "replace this member", got.TerminalReason)
	assert.Equal(t, uint64(1), got.BaselineGeneration)
	assert.Equal(t, "abc", got.BaselineDigest)
}

// TestRolloutHealth_DegradedRules pins what makes a member's config health
// degraded because of the ROLLOUT rather than because of its own config watcher.
func TestRolloutHealth_DegradedRules(t *testing.T) {
	converged := bridge.RolloutStatus{
		MemberID: "node-a", Generation: 4, State: "committed", Applied: true,
	}

	for _, tc := range []struct {
		name   string
		status bridge.RolloutStatus
		want   bool
		reason string
	}{
		{
			name:   "a member running the committed generation is healthy",
			status: converged,
			want:   false,
		},
		{
			name: "committed but not applied is the split cohort, and it degrades",
			status: bridge.RolloutStatus{
				MemberID: "node-a", Generation: 4, State: "committed", Applied: false,
			},
			want:   true,
			reason: "generation 4",
		},
		{
			name: "a confirmed generation this member is not running degrades too",
			status: bridge.RolloutStatus{
				MemberID: "node-a", Generation: 4, State: "confirmed", Applied: false,
			},
			want:   true,
			reason: "generation 4",
		},
		{
			name: "an undecided rollout this member has not applied is normal",
			status: bridge.RolloutStatus{
				MemberID: "node-a", Generation: 4, State: "staging", Applied: false,
			},
			want: false,
		},
		{
			name: "a provisional commit this member has not applied is not divergence",
			status: bridge.RolloutStatus{
				MemberID: "node-a", Generation: 4, State: "committed", Applied: false,
				ConfirmPending: true,
			},
			want: false,
		},
		{
			name: "a stale observation degrades even when the last one looked fine",
			status: bridge.RolloutStatus{
				MemberID: "node-a", Generation: 4, State: "committed", Applied: true,
				Stale: true, ObservationAge: 42 * time.Second, LastError: "store read timed out",
			},
			want:   true,
			reason: "store read timed out",
		},
		{
			name: "a generation whose safe state this member cannot reach degrades",
			status: bridge.RolloutStatus{
				MemberID: "node-a", Generation: 4, State: "reverted", Applied: true,
				TerminalGeneration: 4, TerminalReason: "replace this member",
			},
			want:   true,
			reason: "replace this member",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			degraded, reason := tc.status.DegradedState()
			assert.Equal(t, tc.want, degraded)
			if tc.reason != "" {
				assert.Contains(t, reason, tc.reason,
					"the reason must say what an operator has to act on")
			}
			if !tc.want {
				assert.Empty(t, reason)
			}
		})
	}
}

// TestApp_RolloutHealth_ReachesTheConfigWatchProjection proves the mapping is
// actually wired into the projection the HTTP layer reads, not merely defined.
// That wiring is exactly what was missing: the barrier published divergence
// fields the deep-health block never carried, so a split cohort looked complete
// and healthy on the one page an operator reads.
func TestApp_RolloutHealth_ReachesTheConfigWatchProjection(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(1, "info")), 0o644))

	bcfg := coordinatedBootstrapCfg(t)
	bcfg.ConfigFilePath = cfgPath
	bcfg.PollInterval = "50ms"
	app := NewApp(bcfg,
		WithDynamoDBClient(nil),
		WithClusterRolloutStores(memoryrollout.NewStore(), memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	app.rolloutConfig.PollInterval = 5 * time.Millisecond
	app.rolloutConfig.LeaseTTL = 20 * time.Millisecond

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(2, "debug")), 0o644))
	wait.Until(t, 10*time.Second, "the barrier commits the reload and this member applies it", func() bool {
		return app.CurrentAppliedConfig().Version == 2
	})

	wait.Until(t, 10*time.Second, "deep health publishes the converged rollout observation", func() bool {
		health := app.configWatchHealth()
		return health.Rollout != nil && health.Rollout.Applied
	})
	health := app.configWatchHealth()
	require.NotNil(t, health.Rollout)
	assert.Equal(t, "node-a", health.Rollout.MemberID)
	assert.Equal(t, 2, health.Rollout.ConfigVersion)
	assert.False(t, health.Rollout.Stale, "a live drive observes the row every poll")
	assert.False(t, health.Rollout.ObservedAt.IsZero(), "a successful read stamps the observation instant")
	assert.False(t, health.Degraded, "a member running the committed generation is not degraded")
}

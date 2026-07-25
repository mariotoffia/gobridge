package bootstrap

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryrollout"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// The shipped file-based root (bootstrap.App) hosts the coordinated cluster
// rollout barrier itself (design Phase 6): it builds a bridge.ClusterRolloutDriver
// and, at the reload seam that ADR 0012 used to refuse outright, PROPOSES a
// live-safe delta to the barrier instead of refusing. These are the composition
// tests over memory coordination stores; the ddblocal integration test proves the
// same over real DynamoDB and the real config codec.

func coordinatedBootstrapCfg(t *testing.T) deployinfra.BootstrapConfig {
	t.Helper()
	return deployinfra.BootstrapConfig{
		BridgeID:          "bridge-cluster",
		ConfigFilePath:    t.TempDir() + "/bridge.yaml",
		PollInterval:      "1h", // keep the file watcher dormant
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
		MemberID:          "node-a",
	}
}

// coordinatedLogicalCfg is a minimal coordinated clustered config whose cohort is
// this node alone, so the barrier resolves without a peer. It uses a STATIC
// cluster.endpoints map (which makes IsClusteredDeployment true) rather than
// deployment_mode: clustered, so the App builds a real runtime in-process without
// the ECS endpoint resolver (which needs ECS task metadata) — the coordinated
// rollout path is identical either way.
func coordinatedLogicalCfg(version int) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Version: version,
		Bridge: ports.BridgeSettings{
			ID:       "bridge-cluster",
			LogLevel: "info",
			Cluster: &ports.ClusterConfig{
				Endpoints: map[string]string{"http": "http://127.0.0.1:9999"},
				Rollout:   "coordinated",
				Members:   []string{"node-a"},
			},
		},
	}
}

// TestApp_CoordinatedLiveSafeReload_ProposesToBarrier is the seam: a coordinated
// clustered deployment reloading a live-safe delta PROPOSES it to the rollout
// barrier (a deferral) instead of refusing per ADR 0012, and nothing swaps until
// the barrier commits. This is the whole point of Phase 6 — the shipped root can
// now do coordinated live-safe config changes.
func TestApp_CoordinatedLiveSafeReload_ProposesToBarrier(t *testing.T) {
	store := memoryrollout.NewStore()
	app := NewApp(coordinatedBootstrapCfg(t),
		WithDynamoDBClient(nil),
		WithClusterRolloutStores(store, memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	require.NoError(t, app.buildRolloutDriver(context.Background()))
	require.NotNil(t, app.rolloutDriver, "wired coordination stores must produce a driver")

	base := coordinatedLogicalCfg(1)
	app.appliedRef.Set(base)

	delta := coordinatedLogicalCfg(2)
	delta.Bridge.LogLevel = "debug" // a live-safe change (touches no durable identity)
	err := app.applyLogicalConfig(context.Background(), delta, false)
	require.ErrorIs(t, err, errRolloutDeferred,
		"a coordinated live-safe delta must PROPOSE to the barrier, not refuse or swap")

	r, err := store.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, r.ConfigVersion(), "the delta was proposed as a rollout candidate")
	assert.Same(t, base, app.appliedRef.Get(), "nothing swaps until the barrier commits")
}

// coordinatedConfigYAML is a coordinated clustered config on disk, so the App
// BOOTS coordinated (which is what starts the barrier drive).
func coordinatedConfigYAML(version int, logLevel string) string {
	return fmt.Sprintf(`version: %d
bridge:
  id: bridge-cluster
  log_level: %s
  cluster:
    endpoints:
      http: "http://127.0.0.1:9999"
    rollout: coordinated
    members:
      - node-a
`, version, logLevel)
}

// TestApp_CoordinatedRollout_CommitsAndSwaps is the end-to-end shipped-root proof:
// a file-based App that BOOTED into a coordinated config, given a live-safe reload,
// proposes it to the barrier and — with its own drive running the applier and the
// lease-elected coordinator — commits it and performs the local swap, with nothing
// hand-stepped. This is Phase 6's whole point: the shipped image does coordinated
// live-safe config changes. The cohort is this node alone, a legitimate shape (its
// own ack satisfies the epoch); multi-process coverage is the long-running suite.
func TestApp_CoordinatedRollout_CommitsAndSwaps(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(1, "info")), 0o644))

	bcfg := coordinatedBootstrapCfg(t)
	bcfg.ConfigFilePath = cfgPath
	bcfg.PollInterval = "50ms" // let the file watcher pick up the reload promptly
	app := NewApp(bcfg,
		WithDynamoDBClient(nil),
		WithClusterRolloutStores(memoryrollout.NewStore(), memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	// A cadence short enough that the solo cohort resolves promptly under a real
	// clock (the lease TTL doubles as the coordinator's first-decision lock delay).
	app.rolloutConfig.PollInterval = 5 * time.Millisecond
	app.rolloutConfig.LeaseTTL = 20 * time.Millisecond

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	require.Equal(t, 1, app.CurrentAppliedConfig().Version, "booted on the coordinated v1")

	// Reload a live-safe delta the way production does — write it to the config
	// file. The watcher emits it (advancing the manager's DESIRED to v2), the App
	// proposes it to the barrier, and the drive commits it and performs the local
	// swap. Driving through the watcher (not applyLogicalConfig directly) is what
	// makes the manager reconcile assertion below meaningful.
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(2, "debug")), 0o644))

	wait.Until(t, 10*time.Second, "the barrier commits the file reload and this member applies it", func() bool {
		return app.CurrentAppliedConfig().Version == 2
	})
	assert.Equal(t, "debug", app.CurrentAppliedConfig().Bridge.LogLevel,
		"the committed generation really swapped")

	// Deep health must not read pending after a correct barrier-driven convergence:
	// the manager's DESIRED (from the watch emit) and RUNNING (from AdoptRunning
	// after the barrier swap) are both v2, so ReconfigurePending is false.
	assert.False(t, app.manager.ReconfigurePending(),
		"AdoptRunning re-synced running to the committed config, so health is converged")
}

// TestApp_CoordinatedDeferral_IsApplyInFlight is the Finding-1 regression: a
// coordinated live-safe delta that DEFERS to the barrier must be reported as
// ports.ErrApplyInFlight ("committed, will become running — do NOT roll back"),
// not a definitive failure. The admin config-transaction layer (httpapi) rolls the
// durable write BACK for any non-ErrApplyInFlight apply error — but the barrier
// still commits the deferred delta, so a misclassified deferral would leave the
// durable config and the running runtime permanently split. The file-watch path is
// unaffected (it never rolls back); the classification is what protects the admin
// commit path.
func TestApp_CoordinatedDeferral_IsApplyInFlight(t *testing.T) {
	store := memoryrollout.NewStore()
	app := NewApp(coordinatedBootstrapCfg(t),
		WithDynamoDBClient(nil),
		WithClusterRolloutStores(store, memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	require.NoError(t, app.buildRolloutDriver(context.Background()))

	base := coordinatedLogicalCfg(1)
	app.appliedRef.Set(base)

	delta := coordinatedLogicalCfg(2)
	delta.Bridge.LogLevel = "debug"
	err := app.applyLogicalConfig(context.Background(), delta, false)
	require.ErrorIs(t, err, errRolloutDeferred)
	require.ErrorIs(t, err, ports.ErrApplyInFlight,
		"a coordinated deferral must carry ErrApplyInFlight so an admin commit is committed_not_applied, not rolled back")
}

// TestApp_NonCoordinatedClusteredReload_StillRefuses proves the guard lift is
// narrow: a clustered deployment that did NOT opt into coordinated rollout (no
// driver wired) keeps the ADR 0012 whole-cohort refusal.
func TestApp_NonCoordinatedClusteredReload_StillRefuses(t *testing.T) {
	app := NewApp(coordinatedBootstrapCfg(t),
		WithDynamoDBClient(nil),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	require.Nil(t, app.rolloutDriver, "no coordination stores wired → no driver")

	base := coordinatedLogicalCfg(1)
	base.Bridge.Cluster.Rollout = "" // clustered, but NOT coordinated
	app.appliedRef.Set(base)

	delta := coordinatedLogicalCfg(2)
	delta.Bridge.Cluster.Rollout = ""
	delta.Bridge.LogLevel = "debug"
	err := app.applyLogicalConfig(context.Background(), delta, false)
	require.Error(t, err, "a non-coordinated clustered reload must fail closed (ADR 0012)")
	assert.Contains(t, err.Error(), "clustered")
	assert.NotErrorIs(t, err, errRolloutDeferred, "it is refused, not deferred to a barrier")
}

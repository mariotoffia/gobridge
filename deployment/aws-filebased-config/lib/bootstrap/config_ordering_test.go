package bootstrap

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/config"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// A durable CAS write and its in-band apply are separate operations. Hold the
// version-two apply at that boundary, apply version three, then release version
// two. Driving this ordering synchronously needs no scheduler or timer guesses.
func TestApp_DynamoDBConfig_IgnoresSupersededApply(t *testing.T) {
	for _, newerPath := range []string{"admin", "watcher"} {
		for _, delayedPath := range []string{"admin", "watcher"} {
			t.Run(newerPath+"_before_delayed_"+delayedPath, func(t *testing.T) {
				app := newConfigOrderingApp(t, deployinfra.ConfigSourceDynamoDB)
				source := configOrderingConfig(1, "info")
				app.manager = config.NewManager(config.Layer{
					Name: "dynamodb",
					Loader: configLoaderFunc(func(context.Context) (*ports.BridgeConfig, error) {
						return source, nil
					}),
				})
				boot, err := app.manager.Load(t.Context())
				require.NoError(t, err)
				applyConfigOrderingUpdate(t, app, "watcher", boot)

				source = configOrderingConfig(2, "debug")
				delayed, err := app.manager.Load(t.Context())
				require.NoError(t, err)
				newer := configOrderingConfig(3, "warn")
				if newerPath == "watcher" {
					source = newer
					newer, err = app.manager.Load(t.Context())
					require.NoError(t, err)
				}
				applyConfigOrderingUpdate(t, app, newerPath, newer)
				running := app.CurrentRuntime()
				health := app.configWatchHealth()
				fingerprint := app.lastAppliedFingerprint

				applyConfigOrderingUpdate(t, app, delayedPath, delayed)

				assert.Same(t, running, app.CurrentRuntime(), "a delayed apply must not replace the newer runtime")
				assert.Equal(t, 3, app.CurrentAppliedConfig().Version)
				assert.Equal(t, "warn", app.CurrentAppliedConfig().Bridge.LogLevel)
				assert.Equal(t, 3, app.CurrentLogicalConfig().Version)
				assert.Equal(t, fingerprint, app.lastAppliedFingerprint)
				assert.Equal(t, health, app.configWatchHealth(), "a stale update must not acknowledge success or failure")

				// The next emit acknowledges the actual running version without
				// rebuilding it. In the watcher-first case this is only a replay:
				// no subsequent source change is needed to repair the runtime.
				source = newer
				replay, err := app.manager.Load(t.Context())
				require.NoError(t, err)
				applyConfigOrderingUpdate(t, app, "watcher", replay)
				assert.Same(t, running, app.CurrentRuntime())
				assert.False(t, app.configWatchHealth().Degraded)
				version, ok := app.manager.RunningVersion()
				assert.True(t, ok)
				assert.Equal(t, 3, version)
			})
		}
	}
}

func TestApp_DynamoDBConfig_SupersededApplyPreservesFailure(t *testing.T) {
	for _, delayedPath := range []string{"admin", "watcher"} {
		t.Run(delayedPath, func(t *testing.T) {
			app := newConfigOrderingApp(t, deployinfra.ConfigSourceDynamoDB)
			source := configOrderingConfig(1, "info")
			app.manager = config.NewManager(config.Layer{
				Name: "dynamodb",
				Loader: configLoaderFunc(func(context.Context) (*ports.BridgeConfig, error) {
					return source, nil
				}),
			})
			boot, err := app.manager.Load(t.Context())
			require.NoError(t, err)
			applyConfigOrderingUpdate(t, app, "watcher", boot)
			running := app.CurrentRuntime()
			fingerprint := app.lastAppliedFingerprint

			source = configOrderingConfig(3, "warn")
			rejected, err := app.manager.Load(t.Context())
			require.NoError(t, err)
			app.parameterResolver = staticParameterResolver{}
			applyConfigOrderingUpdate(t, app, "watcher", rejected)
			require.Error(t, app.manager.LastApplyError())
			health := app.configWatchHealth()
			require.True(t, health.Degraded)

			app.parameterResolver = staticParameterResolver{"/admin": "admin-secret-key-123456"}
			applyConfigOrderingUpdate(t, app, delayedPath, configOrderingConfig(2, "debug"))
			assert.Same(t, running, app.CurrentRuntime())
			assert.Equal(t, 1, app.CurrentAppliedConfig().Version)
			assert.Equal(t, 3, app.CurrentLogicalConfig().Version)
			assert.Equal(t, fingerprint, app.lastAppliedFingerprint)
			assert.Equal(t, health, app.configWatchHealth())

			// Failed applies do not consume a version: the same config can
			// recover once its dependency is available, and then replay is a no-op.
			applyConfigOrderingUpdate(t, app, "watcher", rejected)
			assert.Equal(t, 3, app.CurrentAppliedConfig().Version)
			assert.False(t, app.configWatchHealth().Degraded)
			recovered := app.CurrentRuntime()
			applyConfigOrderingUpdate(t, app, "admin", rejected)
			assert.Same(t, recovered, app.CurrentRuntime())
		})
	}
}

func TestApp_FileConfig_AllowsVersionReuseAndRollback(t *testing.T) {
	for _, path := range []string{"admin", "watcher"} {
		t.Run(path, func(t *testing.T) {
			app := newConfigOrderingApp(t, deployinfra.ConfigSourceFile)
			app.manager = config.NewManager(config.Layer{})
			applyConfigOrderingUpdate(t, app, path, configOrderingConfig(3, "info"))
			initial := app.CurrentRuntime()

			applyConfigOrderingUpdate(t, app, path, configOrderingConfig(3, "debug"))
			assert.NotSame(t, initial, app.CurrentRuntime(), "an external edit may reuse the file version")
			assert.Equal(t, "debug", app.CurrentAppliedConfig().Bridge.LogLevel)

			applyConfigOrderingUpdate(t, app, path, configOrderingConfig(2, "warn"))
			assert.Equal(t, 2, app.CurrentLogicalConfig().Version)
			assert.Equal(t, 2, app.CurrentAppliedConfig().Version)
			assert.Equal(t, "warn", app.CurrentAppliedConfig().Bridge.LogLevel)
		})
	}
}

func TestApp_DynamoDBConfig_BarrierCommitRemainsAuthoritative(t *testing.T) {
	for _, tc := range []struct {
		name              string
		source, committed int
	}{
		{"source ahead of committed generation", 3, 2},
		{"committed generation ahead of source", 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newConfigOrderingApp(t, deployinfra.ConfigSourceDynamoDB)
			app.cfg.MemberID = "node-a"
			WithClusterRolloutStores(memoryrollout.NewStore(),
				memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true)))(app)
			require.NoError(t, app.buildRolloutDriver(t.Context()))
			source := coordinatedLogicalCfg(1)
			app.manager = config.NewManager(config.Layer{
				Name: "dynamodb",
				Loader: configLoaderFunc(func(context.Context) (*ports.BridgeConfig, error) {
					return source, nil
				}),
			})
			boot, err := app.manager.Load(t.Context())
			require.NoError(t, err)
			applyConfigOrderingUpdate(t, app, "watcher", boot)
			initial := app.CurrentRuntime()

			source = coordinatedLogicalCfg(tc.source)
			source.Bridge.LogLevel = "debug"
			candidate, err := app.manager.Load(t.Context())
			require.NoError(t, err)
			applyConfigOrderingUpdate(t, app, "watcher", candidate)
			assert.Same(t, initial, app.CurrentRuntime(), "source intake must wait for the barrier")
			assert.True(t, app.manager.ReconfigurePending())
			assert.NoError(t, app.manager.LastApplyError())
			require.ErrorIs(t, app.applyCommittedConfig(t.Context(), candidate), ports.ErrApplyInFlight,
				"a same-version admin apply still defers without rollback")

			// A peer's committed artifact may precede or overtake this node's
			// source observation. The barrier, not source version order, owns
			// the cohort's running config.
			committed := coordinatedLogicalCfg(tc.committed)
			committed.Bridge.LogLevel = "warn"
			app.applyBarrierCommitted(t.Context(), committed)
			assert.Equal(t, tc.committed, app.CurrentAppliedConfig().Version)
			assert.Equal(t, tc.source, app.CurrentLogicalConfig().Version)
			version, ok := app.manager.RunningVersion()
			assert.True(t, ok)
			assert.Equal(t, tc.committed, version)
			assert.True(t, app.manager.ReconfigurePending())
			running := app.CurrentRuntime()
			health := app.configWatchHealth()
			fingerprint := app.lastAppliedFingerprint

			delayed := coordinatedLogicalCfg(2)
			delayed.Bridge.LogLevel = "debug"
			applyConfigOrderingUpdate(t, app, "admin", delayed)
			applyConfigOrderingUpdate(t, app, "watcher", delayed)
			assert.Same(t, running, app.CurrentRuntime())
			assert.Equal(t, tc.source, app.CurrentLogicalConfig().Version)
			assert.Equal(t, fingerprint, app.lastAppliedFingerprint)
			assert.Equal(t, health, app.configWatchHealth())
		})
	}
}

func TestApp_DynamoDBConfig_BootCanUseOlderCommittedConfig(t *testing.T) {
	app := newConfigOrderingApp(t, deployinfra.ConfigSourceDynamoDB)
	source := configOrderingConfig(3, "debug")
	app.manager = config.NewManager(config.Layer{
		Name: "dynamodb",
		Loader: configLoaderFunc(func(context.Context) (*ports.BridgeConfig, error) {
			return source, nil
		}),
	})
	logical, err := app.manager.Load(t.Context())
	require.NoError(t, err)
	app.logicalRef.Set(logical)
	boot := configOrderingConfig(1, "info")

	// Mirror Start after ResolveBoot selects an older committed artifact.
	app.mu.Lock()
	_, err = app.applyLogicalIfChanged(t.Context(), boot, true)
	app.mu.Unlock()
	require.NoError(t, err)
	app.reconcileBootApplyResult(logical, boot)
	assert.Equal(t, 1, app.CurrentAppliedConfig().Version)
	assert.Equal(t, 3, app.CurrentLogicalConfig().Version)
	assert.True(t, app.manager.ReconfigurePending())
}

func TestApp_DynamoDBConfig_RollbackUsesNewVersion(t *testing.T) {
	app := newConfigOrderingApp(t, deployinfra.ConfigSourceDynamoDB)
	applyConfigOrderingUpdate(t, app, "admin", configOrderingConfig(1, "info"))
	app.parameterResolver = staticParameterResolver{}
	require.Error(t, app.applyCommittedConfig(t.Context(), configOrderingConfig(3, "debug")))
	require.Equal(t, 1, app.CurrentAppliedConfig().Version)
	require.Equal(t, 3, app.CurrentLogicalConfig().Version)
	app.parameterResolver = staticParameterResolver{"/admin": "admin-secret-key-123456"}

	// An admin rollback in DynamoDB restores the old content with a NEW CAS
	// version; it must remain applicable despite the newer logical candidate.
	rollback := configOrderingConfig(4, "info")
	applyConfigOrderingUpdate(t, app, "admin", rollback)
	assert.Equal(t, 4, app.CurrentAppliedConfig().Version)
	assert.Equal(t, "info", app.CurrentAppliedConfig().Bridge.LogLevel)
}

func newConfigOrderingApp(t *testing.T, source string) *App {
	t.Helper()
	cfg := dynamoDBSourceConfig()
	cfg.ConfigSource = source
	app := NewApp(cfg, WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))
	app.clk = clocktest.NewAt(time.Unix(0, 0))
	t.Cleanup(func() {
		require.NoError(t, stopRuntime(context.Background(), app.CurrentRuntime(), app.CurrentAppliedConfig()))
		app.closeSupersededHTTP(context.Background(), app.registryRef.Load())
	})
	return app
}

func configOrderingConfig(version int, level string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Version: version,
		Bridge:  ports.BridgeSettings{ID: "bridge-a", LogLevel: level},
	}
}

func applyConfigOrderingUpdate(t *testing.T, app *App, path string, cfg *ports.BridgeConfig) {
	t.Helper()
	if path == "admin" {
		require.NoError(t, app.applyCommittedConfig(t.Context(), cfg),
			"a superseded durable commit must not trigger an admin rollback")
		return
	}
	ch := make(chan *ports.BridgeConfig, 1)
	ch <- cfg
	close(ch)
	app.watchLoop(t.Context(), ch)
}

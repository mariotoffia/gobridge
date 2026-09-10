package bootstrap

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestIntegration_AppCoordinatedRolloutOverDynamoDB is the production-faithful
// proof of the Phase-6 ship: the shipped file-based App, given ONLY a DynamoDB
// client, builds its OWN coordinated rollout barrier — the DynamoDB coordination
// store (created via EnsureTable), the lease store, and the real config codec — and
// drives a live-safe reload through it to a committed swap over REAL DynamoDB. The
// component tests prove the wiring logic over memory stores; what only this proves
// is that the barrier's conditional writes and the durable committed-artifact codec
// round-trip behave the same as actual DynamoDB conditional expressions, through
// the App's own construction path rather than an injected store.
func TestIntegration_AppCoordinatedRolloutOverDynamoDB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	client := ddblocal.Client(t) // skips when DynamoDB Local is not available

	rolloutTable := ddblocal.UniqueTable("app-rollout")
	leaseTable := ddblocal.UniqueTable("app-rollout-lease")
	// The App EnsureTables its OWN coordination (rollout) table; the lease table is
	// deployment-owned (CDK-provisioned in production), so create it here.
	require.NoError(t, dynamodblease.NewStore(client, dynamodblease.WithTableName(leaseTable)).
		EnsureTable(context.Background()))
	ddblocal.CleanupTable(t, client, leaseTable)
	ddblocal.CleanupTable(t, client, rolloutTable)

	cfgPath := t.TempDir() + "/bridge.yaml"
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(1, "info")), 0o644))

	bcfg := coordinatedBootstrapCfg(t)
	bcfg.ConfigFilePath = cfgPath
	bcfg.PollInterval = "50ms"
	bcfg.DynamoDBHARolloutTableName = rolloutTable
	bcfg.DynamoDBHALeaseTableName = leaseTable

	app := NewApp(bcfg,
		WithDynamoDBClient(client), // the App builds the DynamoDB rollout + lease stores itself
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	app.rolloutConfig.PollInterval = 5 * time.Millisecond
	app.rolloutConfig.LeaseTTL = 20 * time.Millisecond

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	wait.Until(t, 20*time.Second, "booted coordinated on real DynamoDB", func() bool {
		cfg := app.CurrentAppliedConfig()
		return cfg != nil && cfg.Version == 1 && !app.manager.ReconfigurePending()
	})

	// Live-safe reload through the config file (production path): propose → the
	// drive commits over real DynamoDB → local swap.
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(2, "debug")), 0o644))
	wait.Until(t, 20*time.Second, "the barrier applies the DynamoDB commit and reconciles manager health", func() bool {
		cfg := app.CurrentAppliedConfig()
		return cfg != nil && cfg.Version == 2 && !app.manager.ReconfigurePending()
	})
	assert.Equal(t, "debug", app.CurrentAppliedConfig().Bridge.LogLevel)
	assert.False(t, app.manager.ReconfigurePending(), "AdoptRunning re-synced the manager over real DynamoDB")

	// The durable committed artifact was written to DynamoDB and decodes back with
	// the App's REAL codec — the Phase-5A round-trip risk, proven through the App.
	// Waited for, not read on the heels of the swap: the artifact write follows the
	// local apply on the drive goroutine and is retried until it verifies, so the
	// generation becoming the running config is NOT the instant the artifact
	// exists. Reading it once here raced that gap and failed with "no committed
	// config artifact" while the write was still in flight.
	rolloutStore := dynamodbrollout.NewStore(client, dynamodbrollout.WithTableName(rolloutTable))
	var committed persistence.CommittedRolloutConfig
	wait.Until(t, 20*time.Second, "the durable committed artifact lands in DynamoDB", func() bool {
		got, err := rolloutStore.CommittedConfig(context.Background())
		if err != nil {
			return false
		}
		committed = got
		return committed.ConfigVersion == 2
	})
	assert.Equal(t, 2, committed.ConfigVersion, "the commit wrote the durable artifact")
	_, decode := app.rolloutCodec()
	got, err := decode(committed.ConfigBytes)
	require.NoError(t, err)
	assert.Equal(t, "debug", got.Bridge.LogLevel, "the real codec decodes the artifact DynamoDB stored")
}

// TestIntegration_AppSeedsAndRecoversTheRolloutBaselineOverDynamoDB proves the
// generation-zero baseline over REAL DynamoDB, through the App's own boot path.
//
// What only this proves is that the seed's monotonic conditional write behaves
// under actual DynamoDB conditional expressions: the first member establishes
// generation zero, and a member restarting after an operator wrote a change the
// cohort has not proposed recovers to that baseline instead of booting the
// uncommitted document its config source hands it.
func TestIntegration_AppSeedsAndRecoversTheRolloutBaselineOverDynamoDB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	client := ddblocal.Client(t) // skips when DynamoDB Local is not available

	rolloutTable := ddblocal.UniqueTable("app-baseline")
	leaseTable := ddblocal.UniqueTable("app-baseline-lease")
	require.NoError(t, dynamodblease.NewStore(client, dynamodblease.WithTableName(leaseTable)).
		EnsureTable(context.Background()))
	ddblocal.CleanupTable(t, client, leaseTable)
	ddblocal.CleanupTable(t, client, rolloutTable)

	cfgPath := t.TempDir() + "/bridge.yaml"
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(1, "info")), 0o600))
	baseline := baselineDigestOnDisk(t, cfgPath)

	newMember := func() *App {
		bcfg := coordinatedBootstrapCfg(t)
		bcfg.ConfigFilePath = cfgPath
		bcfg.PollInterval = "1h"
		bcfg.DynamoDBHARolloutTableName = rolloutTable
		bcfg.DynamoDBHALeaseTableName = leaseTable
		bcfg.DynamoDBHABaselineConfigDigest = baseline
		app := NewApp(bcfg,
			WithDynamoDBClient(client),
			WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
		)
		app.rolloutConfig.PollInterval = 5 * time.Millisecond
		app.rolloutConfig.LeaseTTL = 20 * time.Millisecond
		return app
	}

	first := newMember()
	require.NoError(t, first.Start(t.Context()))
	t.Cleanup(func() { _ = first.Stop(context.Background()) })
	wait.Until(t, 20*time.Second, "first member activates the baseline", func() bool {
		cfg := first.CurrentAppliedConfig()
		return cfg != nil && cfg.Version == 1 && !first.manager.ReconfigurePending()
	})

	rolloutStore := dynamodbrollout.NewStore(client, dynamodbrollout.WithTableName(rolloutTable))
	var committed persistence.CommittedRolloutConfig
	wait.Until(t, 20*time.Second, "generation-zero baseline becomes durable", func() bool {
		got, err := rolloutStore.CommittedConfig(t.Context())
		if err != nil {
			return false
		}
		committed = got
		return committed.ConfigVersion == 1
	})
	assert.Equal(t, uint64(0), committed.Generation)
	require.NoError(t, first.Stop(context.Background()))

	// The operator's change is durably written before any rollout carries it.
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(2, "debug")), 0o600))

	second := newMember()
	require.NoError(t, second.Start(t.Context()))
	t.Cleanup(func() { _ = second.Stop(context.Background()) })
	wait.Until(t, 20*time.Second, "restarted member activates its committed baseline", func() bool {
		cfg := second.CurrentAppliedConfig()
		health := second.configWatchHealth()
		return cfg != nil && cfg.Version == 1 &&
			health.RunningVersion != nil && *health.RunningVersion == 1 &&
			health.DesiredVersion != nil && *health.DesiredVersion == 2 &&
			health.ReconfigurePending
	})
	assert.Equal(t, 1, second.CurrentAppliedConfig().Version,
		"a restart in the write-before-propose window boots the cohort's committed baseline")
	assert.Equal(t, "info", second.CurrentAppliedConfig().Bridge.LogLevel)
	assert.True(t, second.manager.ReconfigurePending(), "uncommitted source version 2 remains distinct from running baseline 1")
}

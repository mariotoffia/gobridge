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
	require.Equal(t, 1, app.CurrentAppliedConfig().Version, "booted coordinated on real DynamoDB")

	// Live-safe reload through the config file (production path): propose → the
	// drive commits over real DynamoDB → local swap.
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(2, "debug")), 0o644))
	wait.Until(t, 20*time.Second, "the barrier commits over real DynamoDB and this member applies it", func() bool {
		return app.CurrentAppliedConfig().Version == 2
	})
	assert.Equal(t, "debug", app.CurrentAppliedConfig().Bridge.LogLevel)
	assert.False(t, app.manager.ReconfigurePending(), "AdoptRunning re-synced the manager over real DynamoDB")

	// The durable committed artifact was written to DynamoDB and decodes back with
	// the App's REAL codec — the Phase-5A round-trip risk, proven through the App.
	rolloutStore := dynamodbrollout.NewStore(client, dynamodbrollout.WithTableName(rolloutTable))
	committed, err := rolloutStore.CommittedConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, committed.ConfigVersion, "the commit wrote the durable artifact")
	_, decode := app.rolloutCodec()
	got, err := decode(committed.ConfigBytes)
	require.NoError(t, err)
	assert.Equal(t, "debug", got.Bridge.LogLevel, "the real codec decodes the artifact DynamoDB stored")
}

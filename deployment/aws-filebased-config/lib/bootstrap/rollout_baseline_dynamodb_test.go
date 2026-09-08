package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// CDK baseline_digest_test.go pins actual per-slot stamps from materialized YAML.
// These tests pass persisted seeder versions through the real DynamoDB loader,
// App startup and committed-artifact codec, with only the remote API replaced.
func TestApp_DynamoDBBaseline_SeedsTheStoredVersion(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		yamlVersion, persisted int
	}{
		{"default fresh seed", 0, 1},
		{"explicit fresh seed", 99, 1},
		{"default overwrite", 0, 8},
		{"explicit overwrite", 99, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deployed := deploymentBaselineConfig(t, tc.yamlVersion)
			stamp, err := bridge.DeploymentBaselineContentDigest(deployed)
			require.NoError(t, err)
			stored := *deployed
			stored.Version = tc.persisted
			store := memoryrollout.NewStore()
			app := dynamoDBBaselineApp(t, store, &stored, stamp)
			require.NoError(t, app.Start(t.Context()))

			committed, err := store.CommittedConfig(t.Context())
			require.NoError(t, err, "a source-assigned version must not prevent generation-zero seeding")
			assert.Equal(t, uint64(0), committed.Generation)
			assert.Equal(t, tc.persisted, committed.ConfigVersion)
			_, decode := app.rolloutCodec()
			artifact, err := decode(committed.ConfigBytes)
			require.NoError(t, err)
			assert.Equal(t, tc.persisted, artifact.Version)
			fullDigest, err := bridge.ConfigArtifactDigest(&stored)
			require.NoError(t, err)
			assert.Equal(t, fullDigest, committed.Digest)
			assert.NotEqual(t, stamp, committed.Digest, "recognition identity is not artifact identity")
			assert.Equal(t, tc.yamlVersion, deployed.Version)
			assert.Equal(t, tc.persisted, app.CurrentAppliedConfig().Version)
			health := app.configWatchHealth()
			require.NotNil(t, health.Rollout)
			assert.Equal(t, committed.Digest, health.Rollout.BaselineDigest)
		})
	}
}

func TestApp_DynamoDBBaseline_RejectsChangedContent(t *testing.T) {
	deployed := deploymentBaselineConfig(t, 99)
	stamp, err := bridge.DeploymentBaselineContentDigest(deployed)
	require.NoError(t, err)
	changed := *deployed
	changed.Version = 8
	changed.Bridge.LogLevel = "debug"
	require.Equal(t, bridge.DeploymentProfileFingerprint(deployed), bridge.DeploymentProfileFingerprint(&changed),
		"profile equality alone must never authorize a deployment baseline")
	store := memoryrollout.NewStore()
	app := dynamoDBBaselineApp(t, store, &changed, stamp)
	require.NoError(t, app.Start(t.Context()))
	_, err = store.CommittedConfig(t.Context())
	assert.ErrorIs(t, err, shared.ErrNotFound, "uncommitted operator content must not become the baseline")
}

func TestApp_DynamoDBBaseline_RestartBeforeProposalRecoversStoredVersion(t *testing.T) {
	deployed := deploymentBaselineConfig(t, 99)
	stamp, err := bridge.DeploymentBaselineContentDigest(deployed)
	require.NoError(t, err)
	stored := *deployed
	stored.Version = 1
	store := memoryrollout.NewStore()
	first := dynamoDBBaselineApp(t, store, &stored, stamp)
	require.NoError(t, first.Start(t.Context()))
	require.NoError(t, first.Stop(t.Context()))

	// The mutable source now holds an operator edit, but no proposal exists yet.
	changed := stored
	changed.Version = 2
	changed.Bridge.LogLevel = "debug"
	_, err = store.Current(t.Context())
	require.ErrorIs(t, err, shared.ErrNotFound)
	restarted := dynamoDBBaselineApp(t, store, &changed, stamp)
	require.NoError(t, restarted.Start(t.Context()))
	assert.Equal(t, 1, restarted.CurrentAppliedConfig().Version)
	assert.Equal(t, "info", restarted.CurrentAppliedConfig().Bridge.LogLevel)
}

func TestApp_DynamoDBBaseline_ReportsFullArtifactIdentity(t *testing.T) {
	deployed := deploymentBaselineConfig(t, 99)
	stamp, err := bridge.DeploymentBaselineContentDigest(deployed)
	require.NoError(t, err)
	stored := *deployed
	stored.Version = 1
	store := memoryrollout.NewStore()
	app := dynamoDBBaselineApp(t, store, &stored, stamp)
	var logs bytes.Buffer
	app.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	require.NoError(t, app.buildRolloutDriver(t.Context()))
	require.NoError(t, app.seedRolloutBaseline(t.Context(), &stored))
	assert.Contains(t, logs.String(), `"outcome":"verified"`)
	assert.NotContains(t, logs.String(), `"outcome":"superseded"`)
	established, err := store.CommittedConfig(t.Context())
	require.NoError(t, err)

	logs.Reset()
	stored.Version = 8 // same deployment content, but a different full artifact
	require.NoError(t, app.seedRolloutBaseline(t.Context(), &stored))
	assert.Contains(t, logs.String(), `"outcome":"superseded"`)
	after, err := store.CommittedConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, established, after, "an established baseline must never be rewritten")
	assert.Equal(t, established.Digest, app.baselineRef.Load().Digest)
}

func TestApp_FileBaseline_RejectsVersionOnlyChange(t *testing.T) {
	store := memoryrollout.NewStore()
	app, path := coordinatedBaselineApp(t, store, coordinatedConfigYAML(0, "info"))
	require.NoError(t, os.WriteFile(path, []byte(coordinatedConfigYAML(1, "info")), 0o600))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	require.NoError(t, app.Start(t.Context()))
	_, err := store.CommittedConfig(t.Context())
	assert.ErrorIs(t, err, shared.ErrNotFound, "file deployments still require the exact source version")
}

func deploymentBaselineConfig(t *testing.T, version int) *ports.BridgeConfig {
	t.Helper()
	yaml := coordinatedConfigYAML(version, "info")
	if version == 0 {
		yaml = strings.TrimPrefix(yaml, "version: 0\n")
	}
	cfg, err := cfgparser.Parse(strings.NewReader(yaml), cfgparser.FormatYAML, newDefaultPluginRegistry())
	require.NoError(t, err)
	return cfg
}

func dynamoDBBaselineApp(t *testing.T, store ports.ClusterRolloutStore, stored *ports.BridgeConfig, stamp string) *App {
	t.Helper()
	data, err := cfgparser.MarshalBridgeConfigJSON(stored)
	require.NoError(t, err)
	response, err := json.Marshal(map[string]any{"Item": map[string]any{
		"PK":      map[string]string{"S": "config#" + stored.Bridge.ID},
		"SK":      map[string]string{"S": "current"},
		"version": map[string]string{"N": fmt.Sprint(stored.Version)},
		"data":    map[string]string{"S": string(data)},
	}})
	require.NoError(t, err)
	client := configDynamoDBClient(t, func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-Amz-Target") != "DynamoDB_20120810.GetItem" {
			t.Errorf("unexpected DynamoDB request: %s", req.Header.Get("X-Amz-Target"))
			return nil, shared.ErrUnavailable
		}
		return configJSONResponse(string(response), http.StatusOK), nil
	})
	bcfg := coordinatedBootstrapCfg(t)
	bcfg.ConfigFilePath = ""
	bcfg.ConfigSource = deployinfra.ConfigSourceDynamoDB
	bcfg.ConfigDynamoDB = &deployinfra.ConfigDynamoDBSettings{TableName: "bridge-config-table"}
	bcfg.DynamoDBHABaselineConfigDigest = stamp
	app := NewApp(bcfg,
		WithDynamoDBClient(client),
		WithClusterRolloutStores(store, memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithCredentialStore(&fakePullStore{}),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	app.clk = clocktest.NewAt(time.Unix(0, 0))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, app.Stop(ctx))
	})
	return app
}

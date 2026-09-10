package bootstrap

import (
	"context"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestApp_DynamoDBSource_LoadErrorStopsStartup verifies source failures propagate
// before a runtime or listener is installed, rather than falling back to a file.
func TestApp_DynamoDBSource_LoadErrorKeepsControlPlane(t *testing.T) {
	cfg := deployinfra.BootstrapConfig{
		BridgeID:         "bridge-config",
		AdminAPIKeyParam: "/admin",
		ConfigSource:     deployinfra.ConfigSourceDynamoDB,
		ConfigDynamoDB:   &deployinfra.ConfigDynamoDBSettings{TableName: "config"},
	}.Normalized()
	require.NoError(t, cfg.Validate(), "the bootstrap schema admits DynamoDB")
	var calls atomic.Int32
	client := configDynamoDBClient(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, "DynamoDB_20120810.GetItem", req.Header.Get("X-Amz-Target"))
		return nil, shared.ErrUnavailable
	})
	cfg.AdminAddr, cfg.MonitorAddr, cfg.TransportHTTPAddr = ":0", ":0", ":0"
	app := NewApp(cfg, WithDynamoDBClient(client),
		WithCredentialStore(&fakePullStore{}), WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	require.NoError(t, app.Start(t.Context()))
	wait.Until(t, time.Second, "source fault reported", func() bool { return app.observationError.Load() != nil })
	assert.Positive(t, calls.Load())
	assert.Nil(t, app.CurrentLogicalConfig(), "must not load an empty file-backed config")
	assert.Nil(t, app.CurrentRuntime())
	assert.NotEmpty(t, app.AdminURL())
	assert.NotEmpty(t, app.TransportURL())
	assert.Same(t, client, app.dynamoDBClient)
}

func TestValidateFilesystemProfile_AdditionalCases(t *testing.T) {
	replicated := deployinfra.BootstrapConfig{
		BridgeID:         "bridge-v",
		ConfigFilePath:   "/tmp/bridge.yaml",
		AdminAPIKeyParam: "/admin",
		Topology:         deployinfra.TopologyFilesystemReplicated,
	}

	single := deployinfra.BootstrapConfig{
		BridgeID:         "bridge-v",
		ConfigFilePath:   "/tmp/bridge.yaml",
		AdminAPIKeyParam: "/admin",
		Topology:         deployinfra.TopologySingle,
	}

	t.Run("rejects route.session under filesystem_replicated topology", func(t *testing.T) {
		err := validateFilesystemProfile(replicated, &ports.BridgeConfig{
			Bridge: ports.BridgeSettings{
				ID:             "bridge-v",
				DeploymentMode: "standalone",
			},
			Routes: []ports.RouteDef{
				{
					ID:         "r1",
					ReceiverID: "rx",
					Bindings:   []string{"b1"},
					Session: &ports.RouteSessionDef{
						SessionID: "sess-1",
						SenderID:  "tx-1",
					},
				},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "route.session lease coordination")
		assert.Contains(t, err.Error(), "does not provision")
	})

	t.Run("allows bridge.cluster.endpoints on filesystem profile", func(t *testing.T) {
		err := validateFilesystemProfile(single, &ports.BridgeConfig{
			Bridge: ports.BridgeSettings{
				ID:             "bridge-v",
				DeploymentMode: "standalone",
				Cluster: &ports.ClusterConfig{
					Endpoints: map[string]string{
						"node-1": "http://node-1:8080",
					},
				},
			},
		})
		require.NoError(t, err)
	})

	t.Run("nil logical config returns error", func(t *testing.T) {
		err := validateFilesystemProfile(single, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "logical config is nil")
	})

	t.Run("non-replicated topology allows shared_outbox route", func(t *testing.T) {
		err := validateFilesystemProfile(single, &ports.BridgeConfig{
			Bridge: ports.BridgeSettings{
				ID:             "bridge-v",
				DeploymentMode: "standalone",
			},
			Routes: []ports.RouteDef{
				{
					ID:           "r1",
					ReceiverID:   "rx",
					Bindings:     []string{"b1"},
					DeliveryMode: "shared_outbox",
				},
			},
		})
		require.NoError(t, err)
	})
}

// TestNewDefaultPluginRegistry_RegistersDynamoDBStoreDecoder asserts the
// bundled plugin registry can decode a DynamoDB store config. Without
// awsstore.Register wired into newDefaultPluginRegistry, Decode returns
// an "unknown plugin kind" error and any bridge.yaml referencing a
// `type: dynamodb` store fails to parse in this AWS deployment profile.
func TestNewDefaultPluginRegistry_RegistersDynamoDBStoreDecoder(t *testing.T) {
	reg := newDefaultPluginRegistry()

	assert.Contains(t, reg.Kinds(), awsstore.DynamoDBKind)

	cfg, err := reg.Decode(awsstore.DynamoDBKind, nil)
	require.NoError(t, err)
	dyn, ok := cfg.(*awsstore.DynamoDBConfig)
	require.True(t, ok, "decoded config should be *awsstore.DynamoDBConfig, got %T", cfg)
	assert.Equal(t, awsstore.DynamoDBKind, dyn.Kind())
}

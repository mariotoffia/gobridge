package bootstrap

import (
	"testing"

	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		assert.Contains(t, err.Error(), "HA/DynamoDB profile")
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

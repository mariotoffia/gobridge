package config

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultMerge_BridgeSettings(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "base-bridge",
			DeploymentMode: "standalone",
			LogLevel:       "info",
		},
	}
	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			DeploymentMode: "clustered",
			LogLevel:       "debug",
		},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	assert.Equal(t, "base-bridge", merged.Bridge.ID, "ID should be preserved from base")
	assert.Equal(t, "clustered", merged.Bridge.DeploymentMode)
	assert.Equal(t, "debug", merged.Bridge.LogLevel)
}

func TestDefaultMerge_BridgeSettings_ZeroNotOverridden(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:       "b1",
			LogLevel: "warn",
		},
	}
	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	assert.Equal(t, "warn", merged.Bridge.LogLevel, "zero overlay should not clear base")
}

func TestDefaultMerge_Stores_OverlayReplacesPerRole(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
	}
	overlay := &ports.BridgeConfig{
		Stores: ports.StoresConfig{
			Lease: func() *ports.StoreConfig {
				sc := &ports.StoreConfig{Type: "dynamodb"}
				sc.SetDecoded(nil, NewRawConfig(map[string]any{"table": "leases"}))
				return sc
			}(),
		},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	assert.Equal(t, "dynamodb", merged.Stores.Lease.Type, "lease should be replaced")
	if opts := rawMap(merged.Stores.Lease.Raw()); assert.NotNil(t, opts, "lease overlay raw options preserved through merge") {
		assert.Equal(t, "leases", opts["table"])
	}
	assert.Equal(t, "memory", merged.Stores.Outbox.Type, "outbox should be preserved")
}

func TestDefaultMerge_SessionsByID(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "mqtt"},
			{ID: "s2", Transport: "mqtt"},
		},
	}
	overlay := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{
			{ID: "s2", Transport: "sqs"},
			{ID: "s3", Transport: "sqs"},
		},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	require.Len(t, merged.Sessions, 3)
	assert.Equal(t, "mqtt", merged.Sessions[0].Transport, "s1 unchanged")
	assert.Equal(t, "sqs", merged.Sessions[1].Transport, "s2 replaced")
	assert.Equal(t, "s3", merged.Sessions[2].ID, "s3 added")
}

func TestDefaultMerge_RoutesByID(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "recv1"},
		},
	}
	overlay := &ports.BridgeConfig{
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "recv2"},
			{ID: "r2", ReceiverID: "recv3"},
		},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	require.Len(t, merged.Routes, 2)
	assert.Equal(t, "recv2", merged.Routes[0].ReceiverID, "r1 replaced")
	assert.Equal(t, "r2", merged.Routes[1].ID)
}

func TestDefaultMerge_HTTP_OverlayReplacesWhole(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		HTTP:   &ports.HTTPConfig{AdminAddr: ":8080", AdminAPIKey: "key1"},
	}
	overlay := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{AdminAddr: ":9090", AdminAPIKey: "key2"},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	assert.Equal(t, ":9090", merged.HTTP.AdminAddr)
	assert.Equal(t, "key2", merged.HTTP.AdminAPIKey)
}

func TestDefaultMerge_HTTP_NilOverlayPreservesBase(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		HTTP:   &ports.HTTPConfig{AdminAddr: ":8080", AdminAPIKey: "key1"},
	}
	overlay := &ports.BridgeConfig{}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	require.NotNil(t, merged.HTTP)
	assert.Equal(t, ":8080", merged.HTTP.AdminAddr)
}

func TestDefaultMerge_ConfigWatch(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge:      ports.BridgeSettings{ID: "b1"},
		ConfigWatch: &ports.ConfigWatchDef{Mode: "notify", Debounce: "100ms"},
	}
	overlay := &ports.BridgeConfig{
		ConfigWatch: &ports.ConfigWatchDef{Mode: "poll", PollInterval: "15s"},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	assert.Equal(t, "poll", merged.ConfigWatch.Mode)
	assert.Equal(t, "15s", merged.ConfigWatch.PollInterval)
}

func TestDefaultMerge_DoesNotMutateBase(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1", LogLevel: "info"},
		Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "mqtt"},
		},
	}
	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{LogLevel: "debug"},
		Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "sqs"},
		},
	}

	_, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	assert.Equal(t, "info", base.Bridge.LogLevel, "base should not be mutated")
	assert.Equal(t, "mqtt", base.Sessions[0].Transport, "base sessions should not be mutated")
}

func TestDefaultMerge_ClusteredWithDistributedOverlay(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge1",
			DeploymentMode: "clustered",
		},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
	}
	overlay := &ports.BridgeConfig{
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "dynamodb"},
			Outbox: &ports.StoreConfig{Type: "dynamodb"},
		},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	assert.Equal(t, "clustered", merged.Bridge.DeploymentMode)
	assert.Equal(t, "dynamodb", merged.Stores.Lease.Type)
	assert.Equal(t, "dynamodb", merged.Stores.Outbox.Type)
}

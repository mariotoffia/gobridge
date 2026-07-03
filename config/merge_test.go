package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
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
				sc.SetDecoded(nil, fakeRawConfig(map[string]any{"table": "leases"}))
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
		HTTP:   &ports.HTTPConfig{AdminAddr: ":8080", AdminAPIKey: shared.NewSecret("key1")},
	}
	overlay := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{AdminAddr: ":9090", AdminAPIKey: shared.NewSecret("key2")},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	assert.Equal(t, ":9090", merged.HTTP.AdminAddr)
	assert.Equal(t, "key2", merged.HTTP.AdminAPIKey.Reveal())
}

func TestDefaultMerge_HTTP_NilOverlayPreservesBase(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		HTTP:   &ports.HTTPConfig{AdminAddr: ":8080", AdminAPIKey: shared.NewSecret("key1")},
	}
	overlay := &ports.BridgeConfig{}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	require.NotNil(t, merged.HTTP)
	assert.Equal(t, ":8080", merged.HTTP.AdminAddr)
}

func TestDefaultMerge_HTTP_RedactedKeyPreservesStoredSecret(t *testing.T) {
	// A client reads the config via the admin GET (which redacts API keys to
	// "[REDACTED]") and PATCHes the HTTP block back to change a non-secret
	// field. The redacted markers must NOT overwrite the real stored keys.
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		HTTP: &ports.HTTPConfig{
			AdminAddr:     ":8080",
			AdminAPIKey:   shared.NewSecret("real-admin-key"),
			MonitorAPIKey: shared.NewSecret("real-monitor-key"),
		},
	}
	overlay := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{
			AdminAddr:     ":9090",
			AdminAPIKey:   shared.RedactedSecret(),             // echoed back from GET
			MonitorAPIKey: shared.NewSecret("new-monitor-key"), // genuinely rotated
		},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)

	assert.Equal(t, ":9090", merged.HTTP.AdminAddr, "non-secret field still merges")
	assert.Equal(t, "real-admin-key", merged.HTTP.AdminAPIKey.Reveal(),
		"redaction marker must not overwrite the stored admin key")
	assert.Equal(t, "new-monitor-key", merged.HTTP.MonitorAPIKey.Reveal(),
		"a genuinely new key must still win")
	assert.False(t, merged.HTTP.AdminAPIKey.IsRedacted(),
		"merged config must never carry the redaction marker as a real value")
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

// TestDefaultMerge_ClusterEndpointsOverlayMerged validates Finding 8: an
// overlay that adds or changes bridge.cluster endpoints must take effect after
// a merge (previously the Cluster block was silently dropped), and the merged
// endpoint map must be cloned so it never aliases the overlay's map.
func TestDefaultMerge_ClusterEndpointsOverlayMerged(t *testing.T) {
	base := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b1"}}
	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			Cluster: &ports.ClusterConfig{
				Endpoints: map[string]string{"node-a": "10.0.0.1:8080"},
			},
		},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.NotNil(t, merged.Bridge.Cluster, "overlay cluster block must be merged, not dropped")
	assert.Equal(t, "10.0.0.1:8080", merged.Bridge.Cluster.Endpoints["node-a"])

	// Mutating the overlay after the merge must not affect the merged config.
	overlay.Bridge.Cluster.Endpoints["node-a"] = "mutated"
	assert.Equal(t, "10.0.0.1:8080", merged.Bridge.Cluster.Endpoints["node-a"],
		"merged cluster endpoints must be cloned, not aliased")
}

// TestDefaultMerge_ClusterNilOverlayPreservesBase validates that a nil overlay
// cluster leaves the base cluster intact (wholesale-replace only when set).
func TestDefaultMerge_ClusterNilOverlayPreservesBase(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:      "b1",
			Cluster: &ports.ClusterConfig{Endpoints: map[string]string{"node-a": "base:8080"}},
		},
	}
	overlay := &ports.BridgeConfig{Bridge: ports.BridgeSettings{LogLevel: "debug"}}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.NotNil(t, merged.Bridge.Cluster)
	assert.Equal(t, "base:8080", merged.Bridge.Cluster.Endpoints["node-a"])
}

// TestDefaultMerge_Version validates the Finding 8 Version merge rule: a
// non-zero overlay version (the newer committed version) wins; a zero overlay
// version leaves the base version intact rather than resetting it to 0.
func TestDefaultMerge_Version(t *testing.T) {
	t.Run("non-zero overlay wins", func(t *testing.T) {
		merged, err := DefaultMerge(
			&ports.BridgeConfig{Version: 3},
			&ports.BridgeConfig{Version: 7},
		)
		require.NoError(t, err)
		assert.Equal(t, 7, merged.Version)
	})

	t.Run("zero overlay preserves base version", func(t *testing.T) {
		merged, err := DefaultMerge(
			&ports.BridgeConfig{Version: 3},
			&ports.BridgeConfig{},
		)
		require.NoError(t, err)
		assert.Equal(t, 3, merged.Version, "a zero overlay version must not reset the base version to 0")
	})
}

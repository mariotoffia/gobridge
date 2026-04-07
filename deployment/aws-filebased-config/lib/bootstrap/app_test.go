package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticParameterResolver map[string]string

func (r staticParameterResolver) ResolveString(_ context.Context, ref string) (string, error) {
	value, ok := r[ref]
	if !ok {
		return "", fmt.Errorf("missing secret ref %s", ref)
	}
	return value, nil
}

func TestApp_StartsWithMissingFileAndServesAdminConfig(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:           "bridge-a",
		ConfigFilePath:     cfgPath,
		PollInterval:       "100ms",
		AdminAddr:          ":0",
		MonitorAddr:        ":0",
		TransportHTTPAddr:  ":0",
		AdminAPIKeyParam:   "/admin",
		MonitorAPIKeyParam: "/monitor",
	}, WithParameterResolver(staticParameterResolver{
		"/admin":   "admin-secret-key-123456",
		"/monitor": "monitor-secret-key-123",
	}))

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() {
		_ = app.Stop(context.Background())
	})

	require.NotNil(t, app.CurrentRuntime())
	require.NotNil(t, app.CurrentLogicalConfig())
	assert.Equal(t, "bridge-a", app.CurrentLogicalConfig().Bridge.ID)

	resp, body := getJSON(t, app.AdminURL()+"/api/v1/admin/config", "admin-secret-key-123456")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	configBody, ok := body["config"].(map[string]any)
	require.True(t, ok)
	bridgeBody, ok := configBody["bridge"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "bridge-a", bridgeBody["id"])
}

func TestApp_ReloadsWhenConfigFileAppearsAndRejectsInvalidChanges(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-b",
		ConfigFilePath:    cfgPath,
		PollInterval:      "100ms",
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}, WithParameterResolver(staticParameterResolver{
		"/admin": "admin-secret-key-123456",
	}))

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() {
		_ = app.Stop(context.Background())
	})

	valid := &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:             "bridge-b",
			DeploymentMode: "standalone",
			LogLevel:       "debug",
		},
	}
	require.NoError(t, config.WriteFile(cfgPath, valid))

	require.Eventually(t, func() bool {
		applied := app.CurrentAppliedConfig()
		return applied != nil && applied.Bridge.LogLevel == "debug"
	}, 3*time.Second, 100*time.Millisecond)

	invalid := &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:             "bridge-b",
			DeploymentMode: "standalone",
		},
		Routes: []config.RouteDef{
			{ID: "broken-route", ReceiverID: "missing", Bindings: []string{"missing"}},
		},
	}
	require.NoError(t, config.WriteFile(cfgPath, invalid))

	time.Sleep(2 * time.Second)
	applied := app.CurrentAppliedConfig()
	require.NotNil(t, applied)
	assert.Equal(t, "debug", applied.Bridge.LogLevel)
}

func TestResolveInputs_InjectsHTTPSecretsWithoutMutatingLogicalConfig(t *testing.T) {
	logical := &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:             "bridge-c",
			DeploymentMode: "standalone",
		},
		Receivers: []config.ReceiverDef{
			{ID: "rx", Transport: "http", Options: map[string]any{"path": "/rx"}},
		},
		Senders: []config.SenderDef{
			{ID: "tx", Transport: "http", Options: map[string]any{"path": "/tx"}},
		},
	}

	inputs, err := resolveInputs(context.Background(), staticParameterResolver{
		"/admin":   "admin-secret-key-123456",
		"/monitor": "monitor-secret-key-123",
		"/rx-key":  "receiver-secret",
		"/tx-key":  "sender-secret",
	}, deployinfra.BootstrapConfig{
		BridgeID:                 "bridge-c",
		ConfigFilePath:           "/tmp/bridge.yaml",
		AdminAPIKeyParam:         "/admin",
		MonitorAPIKeyParam:       "/monitor",
		HTTPReceiverAPIKeyParams: map[string]string{"rx": "/rx-key"},
		HTTPSenderAPIKeyParams:   map[string]string{"tx": "/tx-key"},
		TransportHTTPAddr:        ":0",
	}, logical)
	require.NoError(t, err)

	assert.Equal(t, "admin-secret-key-123456", inputs.AdminAPIKey)
	assert.Equal(t, "monitor-secret-key-123", inputs.MonitorAPIKey)
	assert.Equal(t, "receiver-secret", inputs.RuntimeConfig.Receivers[0].Options["api_key"])
	assert.Equal(t, "sender-secret", inputs.RuntimeConfig.Senders[0].Options["api_key"])
	assert.Nil(t, logical.Receivers[0].Options["api_key"])
	assert.Nil(t, logical.Senders[0].Options["api_key"])
}

func TestResolveInputs_ErrorOnMissingAdminKey(t *testing.T) {
	logical := &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:             "bridge-e",
			DeploymentMode: "standalone",
		},
	}

	_, err := resolveInputs(context.Background(), staticParameterResolver{
		"/monitor": "monitor-secret-key-123",
	}, deployinfra.BootstrapConfig{
		BridgeID:           "bridge-e",
		ConfigFilePath:     "/tmp/bridge.yaml",
		AdminAPIKeyParam:   "/admin",
		MonitorAPIKeyParam: "/monitor",
		TransportHTTPAddr:  ":0",
	}, logical)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing secret ref /admin")
}

func TestResolveInputs_ErrorOnMissingReceiverKey(t *testing.T) {
	logical := &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:             "bridge-f",
			DeploymentMode: "standalone",
		},
		Receivers: []config.ReceiverDef{
			{ID: "rx", Transport: "http", Options: map[string]any{"path": "/rx"}},
		},
	}

	_, err := resolveInputs(context.Background(), staticParameterResolver{
		"/admin":   "admin-key",
		"/monitor": "monitor-key",
	}, deployinfra.BootstrapConfig{
		BridgeID:                 "bridge-f",
		ConfigFilePath:           "/tmp/bridge.yaml",
		AdminAPIKeyParam:         "/admin",
		MonitorAPIKeyParam:       "/monitor",
		HTTPReceiverAPIKeyParams: map[string]string{"rx": "/rx-key"},
		TransportHTTPAddr:        ":0",
	}, logical)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing secret ref /rx-key")
}

func TestValidateFilesystemProfile_RejectsUnsupportedClusterFeatures(t *testing.T) {
	replicated := deployinfra.BootstrapConfig{
		BridgeID:         "bridge-d",
		ConfigFilePath:   "/tmp/bridge.yaml",
		AdminAPIKeyParam: "/admin",
		Topology:         deployinfra.TopologyFilesystemReplicated,
	}

	// Clustered deployment_mode is allowed on filesystem profiles;
	// only features requiring distributed coordination are rejected.
	err := validateFilesystemProfile(replicated, &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:             "bridge-d",
			DeploymentMode: "clustered",
		},
	})
	require.NoError(t, err)

	err = validateFilesystemProfile(replicated, &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:             "bridge-d",
			DeploymentMode: "standalone",
		},
		Routes: []config.RouteDef{
			{ID: "r1", DeliveryMode: "shared_outbox"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared_outbox")
}

func getJSON(t *testing.T, url, apiKey string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp, body
}

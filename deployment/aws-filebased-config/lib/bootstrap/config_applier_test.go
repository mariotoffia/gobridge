package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// TestApp_ConfigApplierAppliesCommitInBand is the FIX 2 regression: the
// filebased bootstrap must wire httpapi's ConfigApplier hook so a config
// committed through the admin transactions API converges the running runtime
// in-band, instead of leaving the committed_not_applied / errConfigApplyFailed
// path dead. The file watcher is kept dormant (poll = 1h), so the only way the
// applied config can change is the in-band applier the App wires in Start.
func TestApp_ConfigApplierAppliesCommitInBand(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	seed := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-applier",
			DeploymentMode: "standalone",
			LogLevel:       "info",
		},
	}
	require.NoError(t, parser.WriteFile(cfgPath, seed))

	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-applier",
		ConfigFilePath:    cfgPath,
		PollInterval:      "1h", // watcher dormant: only the in-band applier can converge
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

	require.Equal(t, "info", app.CurrentAppliedConfig().Bridge.LogLevel)

	const key = "admin-secret-key-123456"
	txnBase := app.AdminURL() + "/api/v1/admin/config/transactions"

	// Begin a transaction.
	resp, body := adminJSON(t, http.MethodPost, txnBase, key, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	txnID, _ := body["txn_id"].(string)
	require.NotEmpty(t, txnID)

	// Overlay bridge.log_level -> debug.
	resp, _ = adminJSON(t, http.MethodPatch, txnBase+"/"+txnID, key, `{"bridge":{"log_level":"debug"}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Commit. The in-band applier must converge the runtime before the commit
	// returns, so the response is a clean "committed" (not committed_not_applied).
	resp, body = adminJSON(t, http.MethodPost, txnBase+"/"+txnID+"/commit", key, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "committed", body["status"])

	// The watcher is dormant, so the applied config could only have changed via
	// the in-band ConfigApplier wired in Start.
	require.Equal(t, "debug", app.CurrentAppliedConfig().Bridge.LogLevel,
		"committed config must be applied in-band, not left to the (dormant) file watcher")
}

// TestApplyCommittedConfig_ReusesReloadPath unit-tests the applier hook in
// isolation: it applies a config through the same reload path the watcher uses
// (applyLogicalConfig) and wraps failures with %w so httpapi can surface
// committed_not_applied.
func TestApplyCommittedConfig_ReusesReloadPath(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-applier-unit",
		ConfigFilePath:    cfgPath,
		PollInterval:      "1h",
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

	// A valid committed config converges the runtime immediately.
	require.NoError(t, app.applyCommittedConfig(t.Context(), &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-applier-unit",
			DeploymentMode: "standalone",
			LogLevel:       "warn",
		},
	}))
	require.Equal(t, "warn", app.CurrentAppliedConfig().Bridge.LogLevel)

	// An invalid committed config is rejected with a wrapped error so the
	// admin API reports committed_not_applied.
	err := app.applyCommittedConfig(t.Context(), &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-applier-unit", DeploymentMode: "standalone"},
		Routes: []ports.RouteDef{{ID: "broken", ReceiverID: "missing", Bindings: []string{"missing"}}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "apply committed config")
}

func adminJSON(t *testing.T, method, url, key, body string) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

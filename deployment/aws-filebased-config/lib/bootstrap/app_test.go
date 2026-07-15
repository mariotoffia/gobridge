package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
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

	valid := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-b",
			DeploymentMode: "standalone",
			LogLevel:       "debug",
		},
	}
	require.NoError(t, parser.WriteFile(cfgPath, valid))

	require.Eventually(t, func() bool {
		applied := app.CurrentAppliedConfig()
		return applied != nil && applied.Bridge.LogLevel == "debug"
	}, 3*time.Second, 100*time.Millisecond)

	invalid := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-b",
			DeploymentMode: "standalone",
		},
		Routes: []ports.RouteDef{
			{ID: "broken-route", ReceiverID: "missing", Bindings: []string{"missing"}},
		},
	}
	require.NoError(t, parser.WriteFile(cfgPath, invalid))

	time.Sleep(2 * time.Second) // SYNC: wait for file watcher to detect and reject invalid config
	applied := app.CurrentAppliedConfig()
	require.NotNil(t, applied)
	assert.Equal(t, "debug", applied.Bridge.LogLevel)
}

// TestApp_AdminConfigEndpointReturnsAppliedNotRejectedReload is the B3
// regression: a reload that fails to apply is written into logicalRef
// (watchLoop does this before calling applyLogicalConfig), but the admin
// config endpoint must surface the *effective* (applied) config, never the
// rejected one. The endpoint reads through ConfigProvider; before the fix it
// pointed at logicalRef, so operators saw a rejected config as if it were
// live. This drives applyLogicalConfig directly (mirroring watchLoop under
// a.mu) so the assertion is deterministic -- no file watcher, no sleep.
func TestApp_AdminConfigEndpointReturnsAppliedNotRejectedReload(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-c",
		ConfigFilePath:    cfgPath,
		PollInterval:      "1h", // keep the file watcher dormant during the test
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

	// Establish a known-good applied config (log_level=debug).
	good := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-c",
			DeploymentMode: "standalone",
			LogLevel:       "debug",
		},
	}
	app.mu.Lock()
	app.logicalRef.Set(good)
	err := app.applyLogicalConfig(t.Context(), good)
	app.mu.Unlock()
	require.NoError(t, err)

	// Simulate a rejected reload exactly as watchLoop does: logicalRef is
	// updated first, then applyLogicalConfig fails (broken route) and the
	// last-good runtime + applied config are kept. The distinct log_level
	// ("error") makes the surfaced config unmistakable.
	rejected := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-c",
			DeploymentMode: "standalone",
			LogLevel:       "error",
		},
		Routes: []ports.RouteDef{
			{ID: "broken-route", ReceiverID: "missing", Bindings: []string{"missing"}},
		},
	}
	app.mu.Lock()
	app.logicalRef.Set(rejected)
	err = app.applyLogicalConfig(t.Context(), rejected)
	app.mu.Unlock()
	require.Error(t, err, "config with a broken route must be rejected")

	// Preconditions: logicalRef holds the rejected config (what the bug
	// surfaced), while appliedRef retains the last-good one.
	require.Equal(t, "error", app.CurrentLogicalConfig().Bridge.LogLevel)
	require.Equal(t, "debug", app.CurrentAppliedConfig().Bridge.LogLevel)

	// The endpoint must return the APPLIED (debug), never the rejected
	// (error) config held in logicalRef.
	resp, body := getJSON(t, app.AdminURL()+"/api/v1/admin/config", "admin-secret-key-123456")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	configBody, ok := body["config"].(map[string]any)
	require.True(t, ok)
	bridgeBody, ok := configBody["bridge"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "debug", bridgeBody["log_level"],
		"admin config endpoint must return the applied config, not the rejected reload")
}

func TestResolveInputs_InjectsHTTPSecretsWithoutMutatingLogicalConfig(t *testing.T) {
	logical := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-c",
			DeploymentMode: "standalone",
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx", Transport: "http", Config: httptransport.Config{Path: "/rx"}},
		},
		Senders: []ports.SenderDef{
			{ID: "tx", Transport: "http", Config: httptransport.Config{Path: "/tx"}},
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
	}, newDefaultPluginRegistry(), logical)
	require.NoError(t, err)

	assert.Equal(t, "admin-secret-key-123456", inputs.AdminAPIKey)
	assert.Equal(t, "monitor-secret-key-123", inputs.MonitorAPIKey)
	rxCfg, ok := inputs.RuntimeConfig.Receivers[0].Config.(httptransport.Config)
	require.True(t, ok)
	assert.Equal(t, "receiver-secret", rxCfg.APIKey.Reveal())
	txCfg, ok := inputs.RuntimeConfig.Senders[0].Config.(httptransport.Config)
	require.True(t, ok)
	assert.Equal(t, "sender-secret", txCfg.APIKey.Reveal())
	logicalRx, _ := logical.Receivers[0].Config.(httptransport.Config)
	assert.Equal(t, "", logicalRx.APIKey.Reveal(), "logical config must not be mutated")
	logicalTx, _ := logical.Senders[0].Config.(httptransport.Config)
	assert.Equal(t, "", logicalTx.APIKey.Reveal(), "logical config must not be mutated")
}

func TestResolveInputs_ErrorOnMissingAdminKey(t *testing.T) {
	logical := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
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
	}, newDefaultPluginRegistry(), logical)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing secret ref /admin")
}

func TestResolveInputs_ErrorOnMissingReceiverKey(t *testing.T) {
	logical := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-f",
			DeploymentMode: "standalone",
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx", Transport: "http", Config: httptransport.Config{Path: "/rx"}},
		},
	}

	_, err := resolveInputs(context.Background(), staticParameterResolver{
		"/admin":   "admin-key-0123456",
		"/monitor": "monitor-secret-key-123",
	}, deployinfra.BootstrapConfig{
		BridgeID:                 "bridge-f",
		ConfigFilePath:           "/tmp/bridge.yaml",
		AdminAPIKeyParam:         "/admin",
		MonitorAPIKeyParam:       "/monitor",
		HTTPReceiverAPIKeyParams: map[string]string{"rx": "/rx-key"},
		TransportHTTPAddr:        ":0",
	}, newDefaultPluginRegistry(), logical)

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
	err := validateFilesystemProfile(replicated, &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-d",
			DeploymentMode: "clustered",
		},
	})
	require.NoError(t, err)

	err = validateFilesystemProfile(replicated, &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-d",
			DeploymentMode: "standalone",
		},
		Routes: []ports.RouteDef{
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
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp, body
}

// TestClusteredReload proves the H8 fail-closed guard on the AWS composition
// root: a live reload of (or INTO) a CLUSTERED deployment is refused via the
// existing reload-failure path (applyLogicalConfig returns an error, so
// watchLoop keeps the last-good runtime and applyCommittedConfig surfaces
// committed_not_applied), keeping the running runtime and applied config
// untouched. A genuine no-op re-emit is detected before the guard and stays
// accepted (skipped).
func TestClusteredReload(t *testing.T) {
	newStartedApp := func(t *testing.T) *App {
		cfgPath := t.TempDir() + "/bridge.yaml"
		app := NewApp(deployinfra.BootstrapConfig{
			BridgeID:          "bridge-cluster",
			ConfigFilePath:    cfgPath,
			PollInterval:      "1h", // keep the file watcher dormant
			AdminAddr:         ":0",
			MonitorAddr:       ":0",
			TransportHTTPAddr: ":0",
			AdminAPIKeyParam:  "/admin",
		}, WithParameterResolver(staticParameterResolver{
			"/admin": "admin-secret-key-123456",
		}))
		require.NoError(t, app.Start(t.Context()))
		t.Cleanup(func() { _ = app.Stop(context.Background()) })
		return app
	}

	standalone := func() *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Version: 3,
			Bridge:  ports.BridgeSettings{ID: "bridge-cluster", DeploymentMode: "standalone", LogLevel: "info"},
		}
	}
	clustered := func() *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Version: 5,
			Bridge:  ports.BridgeSettings{ID: "bridge-cluster", DeploymentMode: "clustered", LogLevel: "info"},
		}
	}

	reload := func(t *testing.T, app *App, cfg *ports.BridgeConfig) error {
		app.mu.Lock()
		defer app.mu.Unlock()
		app.logicalRef.Set(cfg)
		return app.applyLogicalConfig(t.Context(), cfg)
	}

	t.Run("proposed clustered deployment refuses a live reload", func(t *testing.T) {
		app := newStartedApp(t)
		// Establish a known-good standalone applied config.
		require.NoError(t, reload(t, app, standalone()))
		oldRt := app.CurrentRuntime()
		require.NotNil(t, oldRt)
		// Capture the EXACT applied reference and version before the rejected
		// reload so we can prove neither is mutated (finding 2).
		beforeApplied := app.CurrentAppliedConfig()
		require.NotNil(t, beforeApplied)

		err := reload(t, app, clustered())
		require.Error(t, err, "reloading a standalone runtime INTO a clustered config must fail closed")
		assert.Contains(t, err.Error(), "clustered")
		assert.Same(t, oldRt, app.CurrentRuntime(), "the running runtime must be untouched")
		assert.Same(t, beforeApplied, app.CurrentAppliedConfig(),
			"the applied reference pointer must be unchanged by a refused reload")
		assert.Equal(t, "standalone", app.CurrentAppliedConfig().Bridge.DeploymentMode,
			"the applied config must remain the standalone one")
		assert.Equal(t, 3, app.CurrentAppliedConfig().Version,
			"the applied version must not advance to the rejected clustered version")
	})

	t.Run("current clustered deployment refuses a live reload", func(t *testing.T) {
		app := newStartedApp(t)
		oldRt := app.CurrentRuntime()
		require.NotNil(t, oldRt)
		// Simulate a currently-clustered applied config (a fresh boot INTO
		// clustered is legitimate; only a live reload is guarded).
		appliedClustered := clustered()
		app.mu.Lock()
		app.appliedRef.Set(appliedClustered)
		app.mu.Unlock()

		err := reload(t, app, standalone())
		require.Error(t, err, "leaving a clustered cohort via live reload must fail closed")
		assert.Contains(t, err.Error(), "clustered")
		assert.Same(t, oldRt, app.CurrentRuntime(), "the running runtime must be untouched")
		assert.Same(t, appliedClustered, app.CurrentAppliedConfig(),
			"the applied reference pointer must be unchanged by a refused reload")
		assert.Equal(t, "clustered", app.CurrentAppliedConfig().Bridge.DeploymentMode,
			"the applied config must be unchanged")
		assert.Equal(t, 5, app.CurrentAppliedConfig().Version,
			"the applied version must not change on a refused reload")
	})

	t.Run("committed clustered reload surfaces committed_not_applied", func(t *testing.T) {
		// Finding 2: exercise the EXISTING committed failure path. An admin commit
		// that carries a clustered reload must be rejected through
		// applyCommittedConfig (which wraps the guard error as
		// committed_not_applied), leaving runtime, applied reference/version, and
		// the recorded fingerprint untouched — disk and runtime diverge until an
		// externally coordinated cohort rollout, never a silent per-process swap.
		app := newStartedApp(t)
		require.NoError(t, reload(t, app, standalone()))
		oldRt := app.CurrentRuntime()
		require.NotNil(t, oldRt)
		beforeApplied := app.CurrentAppliedConfig()
		require.NotNil(t, beforeApplied)
		app.mu.Lock()
		beforeFingerprint := app.lastAppliedFingerprint
		app.mu.Unlock()

		err := app.applyCommittedConfig(t.Context(), clustered())
		require.Error(t, err, "a committed clustered reload must be rejected")
		assert.Contains(t, err.Error(), "apply committed config",
			"the failure must surface through the committed-not-applied path")
		assert.Contains(t, err.Error(), "clustered")
		assert.Same(t, oldRt, app.CurrentRuntime(), "the running runtime must be untouched")
		assert.Same(t, beforeApplied, app.CurrentAppliedConfig(),
			"the applied reference must be unchanged by a rejected committed reload")
		assert.Equal(t, 3, app.CurrentAppliedConfig().Version,
			"the applied version must not advance to the rejected clustered version")
		app.mu.Lock()
		afterFingerprint := app.lastAppliedFingerprint
		app.mu.Unlock()
		assert.Equal(t, beforeFingerprint, afterFingerprint,
			"a rejected reload must not record the clustered config's fingerprint")
	})

	t.Run("no-op clustered reload stays accepted", func(t *testing.T) {
		app := newStartedApp(t)
		cc := clustered()
		fp := app.parsedFingerprint(cc, true)
		require.NotEmpty(t, fp)

		app.mu.Lock()
		app.appliedRef.Set(cc)
		app.lastAppliedFingerprint = fp
		skipped, err := app.applyLogicalIfChanged(t.Context(), cc, true)
		app.mu.Unlock()

		require.NoError(t, err, "a no-op clustered reload must not be refused")
		assert.True(t, skipped, "an identical clustered re-emit is a no-op detected before the guard")
	})
}

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	testAdminAPIKey   = "test-admin-key-minimum-16chars"
	testWrongAPIKey   = "wrong-admin-key-minimum-16chars"
	testMonitorAPIKey = "test-monitor-key-min-16chars"
)

// ---------------------------------------------------------------------------
// Server setup helpers
// ---------------------------------------------------------------------------

// configAPIServer holds the pieces of a running config API test server.
type configAPIServer struct {
	URL            string
	ConfigFilePath string
	Server         *httpapi.Server
	Runtime        *goruntime.Runtime
}

// newConfigAPITestServer starts a real httpapi.Server on a random port
// with config management enabled. It does NOT start a supervisor or
// file watcher — use newConfigAPITestServerWithPipeline for that.
func newConfigAPITestServer(t *testing.T, baseCfg *ports.BridgeConfig) configAPIServer {
	t.Helper()

	dir := t.TempDir()
	cfgPath := fmt.Sprintf("%s/config.yaml", dir)
	if err := config.WriteFile(cfgPath, baseCfg); err != nil {
		t.Fatalf("write base config: %v", err)
	}

	currentCfg := baseCfg
	rt := goruntime.New(goruntime.WithInstanceID("config-api-test"))
	apiCfg := httpapi.Config{
		AdminAddr:       ":0",
		MonitorAddr:     ":0",
		AdminAPIKey:     testAdminAPIKey,
		MonitorAPIKey:   testMonitorAPIKey,
		RuntimeProvider: func() *goruntime.Runtime { return rt },
		ConfigStore:     &config.FileStore{Path: cfgPath, Registry: newTestRegistry()},
		ConfigProvider:  func() *ports.BridgeConfig { return currentCfg },
	}

	srv := httpapi.New(rt, apiCfg, httpapi.WithServerLogger(nil))
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(t.Context()) })

	url := srv.AdminURL()
	return configAPIServer{
		URL:            url,
		ConfigFilePath: cfgPath,
		Server:         srv,
		Runtime:        rt,
	}
}

// configAPIPipelineServer extends configAPIServer with supervisor access.
type configAPIPipelineServer struct {
	configAPIServer
	Supervisor *bridge.Supervisor
	SwapEvents chan bridge.SwapEvent
}

// newConfigAPITestServerWithPipeline starts the full pipeline: file
// watcher → config manager → supervisor → runtime, plus the HTTP
// server. Uses poll-mode watcher (100ms) for deterministic testing.
func newConfigAPITestServerWithPipeline(t *testing.T, baseCfg *ports.BridgeConfig) configAPIPipelineServer {
	t.Helper()

	dir := t.TempDir()
	cfgPath := fmt.Sprintf("%s/config.yaml", dir)
	if err := config.WriteFile(cfgPath, baseCfg); err != nil {
		t.Fatalf("write base config: %v", err)
	}

	// File source + poll-mode watcher (100ms for fast test feedback).
	reg := newTestRegistry()
	fileSource := fileconfig.NewSource(cfgPath, reg)
	watcher := fileconfig.NewWatcher(cfgPath, reg,
		fileconfig.WithMode(fileconfig.ModePoll),
		fileconfig.WithPollInterval(100*time.Millisecond),
	)

	mgr := config.NewManager(
		config.Layer{Name: "file", Loader: fileSource, Watcher: watcher},
	)
	t.Cleanup(mgr.Stop)

	ctx := t.Context()
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}

	swapEvents := make(chan bridge.SwapEvent, 10)
	sup := bridge.NewSupervisor(
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) {
			select {
			case swapEvents <- ev:
			default:
			}
		}),
	)
	sup.RegisterTransport("fake", &cfgFakeTransportFactory{})
	sup.RegisterStoreFactory("memory", &cfgFakeStoreFactory{})

	watchCh, err := mgr.Watch(ctx)
	if err != nil {
		t.Fatalf("start watch: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- sup.Run(ctx, cfg, watchCh) }()

	rt := waitForSupervisorRuntime(t, sup, 5*time.Second)

	apiCfg := httpapi.Config{
		AdminAddr:       ":0",
		MonitorAddr:     ":0",
		AdminAPIKey:     testAdminAPIKey,
		MonitorAPIKey:   testMonitorAPIKey,
		RuntimeProvider: sup.Runtime,
		ConfigStore:     &config.FileStore{Path: cfgPath, Registry: newTestRegistry()},
		ConfigProvider:  sup.Config,
	}

	srv := httpapi.New(rt, apiCfg, httpapi.WithServerLogger(nil))
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(t.Context()) })

	url := srv.AdminURL()
	return configAPIPipelineServer{
		configAPIServer: configAPIServer{
			URL:            url,
			ConfigFilePath: cfgPath,
			Server:         srv,
			Runtime:        rt,
		},
		Supervisor: sup,
		SwapEvents: swapEvents,
	}
}

// ---------------------------------------------------------------------------
// HTTP client helpers
// ---------------------------------------------------------------------------

func apiGet(t *testing.T, url, apiKey string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp, decodeJSON(t, resp)
}

func apiPost(t *testing.T, url, apiKey string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(http.MethodPost, url, r)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp, decodeJSON(t, resp)
}

func apiPatch(t *testing.T, url, apiKey string, body any) (*http.Response, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(data))
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	return resp, decodeJSON(t, resp)
}

func apiDelete(t *testing.T, url, apiKey string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	return resp, decodeJSON(t, resp)
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

// ---------------------------------------------------------------------------
// Transaction shortcut helpers
// ---------------------------------------------------------------------------

func createTransaction(t *testing.T, serverURL, apiKey string) string {
	t.Helper()
	resp, body := apiPost(t, serverURL+"/api/v1/admin/config/transactions", apiKey, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create txn: got %d, body: %v", resp.StatusCode, body)
	}
	return body["txn_id"].(string)
}

func applyOverlay(t *testing.T, serverURL, apiKey, txnID string, overlay any) map[string]any {
	t.Helper()
	url := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", serverURL, txnID)
	resp, body := apiPatch(t, url, apiKey, overlay)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch txn: got %d, body: %v", resp.StatusCode, body)
	}
	return body
}

func commitTransaction(t *testing.T, serverURL, apiKey, txnID string) {
	t.Helper()
	url := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s/commit", serverURL, txnID)
	resp, body := apiPost(t, url, apiKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commit txn: got %d, body: %v", resp.StatusCode, body)
	}
}

func rollbackTransaction(t *testing.T, serverURL, apiKey, txnID string) {
	t.Helper()
	url := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", serverURL, txnID)
	resp, body := apiDelete(t, url, apiKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rollback txn: got %d, body: %v", resp.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// Poll helpers
// ---------------------------------------------------------------------------

func pollForCondition(t *testing.T, timeout, interval time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(interval) // SYNC: poll for condition
	}
	t.Fatal("timed out waiting for condition")
}

func pollForSupervisorRoute(t *testing.T, sup *bridge.Supervisor, routeID string, timeout time.Duration) {
	t.Helper()
	pollForCondition(t, timeout, 20*time.Millisecond, func() bool {
		rt := sup.Runtime()
		if rt == nil {
			return false
		}
		for _, r := range rt.Routes() {
			if r.ID == routeID {
				return true
			}
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// Config helpers
// ---------------------------------------------------------------------------

func readConfigFromDisk(t *testing.T, path string) *ports.BridgeConfig {
	t.Helper()
	cfg, err := config.ParseFile(path, config.FormatYAML, newTestRegistry())
	if err != nil {
		t.Fatalf("parse config from disk: %v", err)
	}
	return cfg
}

func readRawFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return data
}

// baseConfigForAPI returns a minimal valid BridgeConfig suitable for
// config API integration tests.
func baseConfigForAPI() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              "api-test-bridge",
			DeploymentMode:  "standalone",
			ShutdownTimeout: "5s",
			DrainTimeout:    "1s",
			LogLevel:        "info",
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx-1", Transport: "fake"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx-1", Transport: "fake"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bind-1", SenderID: "tx-1", Address: "addr/1"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx-1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"bind-1"},
			},
		},
	}
}

// baseConfigForAPIWithHTTP returns a config with HTTP block (for
// redaction tests).
func baseConfigForAPIWithHTTP() *ports.BridgeConfig {
	cfg := baseConfigForAPI()
	cfg.HTTP = &ports.HTTPConfig{
		AdminAddr:     ":8080",
		MonitorAddr:   ":8081",
		AdminAPIKey:   "real-secret-admin-key-1234",
		MonitorAPIKey: "real-secret-monitor-key-5678",
	}
	return cfg
}

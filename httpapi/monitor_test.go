package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestHandleHealth_WithComponentErrors validates that the health endpoint
// returns an unhealthy response with a component_errors map when the runtime
// reports component failures.
func TestHandleHealth_WithComponentErrors(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-health-errors"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/health", nil)
	s.handleHealth(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "not_running", body["status"])
}

// TestHandleHealth_CacheControl validates the Cache-Control header is set
// on health probe responses.
func TestHandleHealth_CacheControl(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-health-cache"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/health", nil)
	s.handleHealth(rec, req)

	assert.Equal(t, "no-cache, max-age=0", rec.Header().Get("Cache-Control"))
}

// TestHandleLive_ReturnsAlive validates the liveness probe returns "alive".
func TestHandleLive_ReturnsAlive(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-live"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil)
	s.handleLive(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "alive", body["status"])
}

// terminalRuntimeStub satisfies ports.Runtime by embedding the interface and
// overriding only Terminal — the sole method handleLive calls. Any other call
// would panic on the nil embedded interface, which keeps the test honest about
// handleLive's dependencies.
type terminalRuntimeStub struct{ ports.Runtime }

func (terminalRuntimeStub) Terminal() bool { return true }

// TestHandleLive_TerminalReturns503 validates the liveness probe fails closed
// when the runtime is terminal (an unrecoverable component failure that
// cancelled the runtime). Failing liveness is what makes Kubernetes restart a
// dead-but-running process instead of leaving it wedged.
func TestHandleLive_TerminalReturns503(t *testing.T) {
	s := New(terminalRuntimeStub{}, testConfig())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil)
	s.handleLive(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "terminal", body["status"])
}

// TestMonitorHandleReady_NotRunning validates the readiness probe returns 503
// when the bridge is not running.
func TestMonitorHandleReady_NotRunning(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-ready"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil)
	s.handleReady(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestHandleTopology_ConfigVersion validates the fleet-convergence field
// (A6-R1): GET /topology surfaces the running config_version when a
// ConfigProvider is wired, and omits it entirely when none is configured so
// a missing provider is never confused with config version 0.
func TestHandleTopology_ConfigVersion(t *testing.T) {
	t.Run("present when config provider is wired", func(t *testing.T) {
		rt := runtime.New(runtime.WithInstanceID("topo-cfgver"))
		cfg := testConfig()
		cfg.ConfigProvider = func() *ports.BridgeConfig {
			return &ports.BridgeConfig{Version: 7}
		}
		s := New(rt, cfg)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
		s.handleTopology(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, float64(7), body["config_version"],
			"config_version must reflect the version returned by the ConfigProvider")
	})

	t.Run("omitted when no config provider is wired", func(t *testing.T) {
		rt := runtime.New(runtime.WithInstanceID("topo-nocfg"))
		s := New(rt, testConfig()) // testConfig wires no ConfigProvider

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
		s.handleTopology(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		_, present := body["config_version"]
		assert.False(t, present, "config_version must be omitted when no ConfigProvider is wired")
	})
}

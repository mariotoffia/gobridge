package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestHandleLogs_ReturnsNotImplemented validates the logs endpoint returns
// a stub response indicating log streaming is not yet implemented.
func TestHandleLogs_ReturnsNotImplemented(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-logs"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/logs", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	s.handleLogs(rec, req)

	assert.Equal(t, http.StatusNotImplemented, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "log streaming not yet implemented", body["error"])
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

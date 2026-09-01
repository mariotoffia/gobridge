package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/runtime"
)

func testRuntimeForMonitor() *runtime.Runtime {
	return runtime.New(runtime.WithInstanceID("test-instance"))
}

// TestHandleReady_RoleStandalone proves the legacy probe reports the instance
// role once the bridge is actually ready. It needs a runtime that CARRIES a
// route: an instance with no routes and no sessions bridges nothing and is
// capped below the LevelFull the legacy probe gates on.
func TestHandleReady_RoleStandalone(t *testing.T) {
	rt := newBridgingRuntime(t, "test-instance")

	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: shared.NewSecret("key-0123456789abcdef")})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil)
	s.MonitorMux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "ready", body["status"])
	assert.Equal(t, "standalone", body["role"])
}

// TestHandleReady_MethodNotAllowed verifies POST returns 405 Method Not Allowed.
func TestHandleReady_MethodNotAllowed(t *testing.T) {
	rt := testRuntimeForMonitor()
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: shared.NewSecret("key-0123456789abcdef")})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/monitor/ready", nil)
	s.MonitorMux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandleReady_NotRunning_NoRole(t *testing.T) {
	rt := testRuntimeForMonitor()
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: shared.NewSecret("key-0123456789abcdef")})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil)
	s.MonitorMux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "not ready", body["error"])
	_, hasRole := body["role"]
	assert.False(t, hasRole, "503 response must not include role")
}

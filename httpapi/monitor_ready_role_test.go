package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRuntimeForMonitor() *runtime.Runtime {
	return runtime.New(runtime.WithInstanceID("test-instance"))
}

func TestHandleReady_RoleStandalone(t *testing.T) {
	rt := testRuntimeForMonitor()
	ctx := context.Background()
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() {
		_ = rt.Stop(context.Background())
	})

	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: "key-0123456789abcdef"})

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
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: "key-0123456789abcdef"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/monitor/ready", nil)
	s.MonitorMux().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandleReady_NotRunning_NoRole(t *testing.T) {
	rt := testRuntimeForMonitor()
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: "key-0123456789abcdef"})

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

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

func testRuntime() *runtime.Runtime {
	return runtime.New(runtime.WithInstanceID("test-instance"))
}

func TestHandleBridge(t *testing.T) {
	rt := testRuntime()
	s := New(rt, DefaultConfig())

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "test-instance", body["instance_id"])
	assert.Equal(t, false, body["running"])
}

func TestHandleRoutes_Empty(t *testing.T) {
	rt := testRuntime()
	s := New(rt, DefaultConfig())

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/routes", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var routes []routeView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &routes))
	assert.Empty(t, routes)
}

func TestHandleHealth(t *testing.T) {
	rt := testRuntime()
	s := New(rt, DefaultConfig())

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "not_running", body["status"])
}

func TestHandleLive(t *testing.T) {
	rt := testRuntime()
	s := New(rt, DefaultConfig())

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandleReady_NotRunning(t *testing.T) {
	rt := testRuntime()
	s := New(rt, DefaultConfig())

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRequireAuth(t *testing.T) {
	rt := testRuntime()
	cfg := DefaultConfig()
	cfg.APIKey = "secret"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	t.Run("missing key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("valid X-API-Key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("X-API-Key", "secret")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("valid Bearer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestMethodNotAllowed(t *testing.T) {
	rt := testRuntime()
	s := New(rt, DefaultConfig())

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/bridge", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestDLQ_NoStore(t *testing.T) {
	rt := testRuntime()
	s := New(rt, DefaultConfig())

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/dlq", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body["status"], "no DLQ")
}

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func adminRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-API-Key", "test-secret")
	return req
}

// TestHandleStart_StartsRuntime validates that POST /admin/bridge/start starts the bridge.
func TestHandleStart_StartsRuntime(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-start"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	s.handleStart(rec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/start"))

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "started", body["status"])
}

// TestHandleStart_AlreadyRunning validates that starting an already-running
// bridge returns a conflict error.
func TestHandleStart_AlreadyRunning(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-already"))
	s := New(rt, testConfig())

	ctx := context.Background()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(ctx) }()

	rec := httptest.NewRecorder()
	s.handleStart(rec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/start"))

	assert.Equal(t, http.StatusConflict, rec.Code)
}

// TestHandleStop_StopsRuntime validates that POST /admin/bridge/stop stops a running bridge.
func TestHandleStop_StopsRuntime(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-stop"))
	s := New(rt, testConfig())

	ctx := context.Background()
	require.NoError(t, rt.Start(ctx))

	rec := httptest.NewRecorder()
	s.handleStop(rec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/stop"))

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "stopped", body["status"])
}

// TestHandleStop_NotRunning validates that stopping when not running returns success
// (the runtime.Stop is a no-op in that case).
func TestHandleStop_NotRunning(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-stop-noop"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	s.handleStop(rec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/stop"))

	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestHandleDLQMessages_NoStore validates that DLQ messages endpoint returns
// 404 when no DLQ store is configured.
func TestHandleDLQMessages_NoStore(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-dlq-nostore"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	s.handleDLQMessages(rec, adminRequest(http.MethodGet, "/api/v1/admin/dlq/messages"))

	assert.Equal(t, http.StatusNotFound, rec.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Contains(t, body["error"], "no DLQ store")
}

// TestHandleStart_MethodNotAllowed validates that GET on /bridge/start is rejected.
func TestHandleStart_MethodNotAllowed(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-method"))
	s := New(rt, testConfig())

	rec := httptest.NewRecorder()
	s.handleStart(rec, adminRequest(http.MethodGet, "/api/v1/admin/bridge/start"))

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func adminRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	return req
}

// adminStopLeakMetrics fails Flush so Runtime.Stop returns a non-nil error
// without relying on request context cancellation races.
type adminStopLeakMetrics struct{}

func (adminStopLeakMetrics) Counter(string, int64, ...domain.Tag)       {}
func (adminStopLeakMetrics) Gauge(string, float64, ...domain.Tag)       {}
func (adminStopLeakMetrics) Histogram(string, float64, ...domain.Tag)   {}
func (adminStopLeakMetrics) Timer(string, time.Duration, ...domain.Tag) {}
func (adminStopLeakMetrics) Close(context.Context) error                { return nil }
func (adminStopLeakMetrics) Flush(context.Context) error {
	return fmt.Errorf("INTERNAL_STOP_SECRET_do_not_expose")
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

// TestHandleStart_MethodNotAllowed validates that GET on /bridge/start is rejected
// at the mux level (method-prefix pattern enforcement).
func TestHandleStart_MethodNotAllowed(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-method"))
	s := New(rt, testConfig())

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, adminRequest(http.MethodGet, "/api/v1/admin/bridge/start"))

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestAdmin_StartError_DoesNotLeakInternalError(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-start-leak"))
	s := New(rt, testConfig())

	ctx := context.Background()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(ctx) }()

	rec := httptest.NewRecorder()
	s.handleStart(rec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/start"))

	require.Equal(t, http.StatusConflict, rec.Code)
	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "bridge start failed", body["error"])
	assert.NotContains(t, strings.ToLower(rec.Body.String()), "already running")
}

func TestAdmin_StopError_DoesNotLeakInternalError(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("test-stop-leak"),
		runtime.WithMetrics(adminStopLeakMetrics{}),
	)
	s := New(rt, testConfig())

	ctx := context.Background()
	require.NoError(t, rt.Start(ctx))
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	}()

	rec := httptest.NewRecorder()
	s.handleStop(rec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/stop"))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "bridge stop failed", body["error"])
	assert.NotContains(t, rec.Body.String(), "INTERNAL_STOP_SECRET")
}

func TestAdmin_InjectError_DoesNotLeakInternalError(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-inject-leak"))
	s := New(rt, testConfig())

	bodyJSON := `{"subject":"s"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/r1/inject", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	req.SetPathValue("routeID", "r1")

	rec := httptest.NewRecorder()
	s.handleInject(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "message injection failed", body["error"])
	assert.NotContains(t, strings.ToLower(rec.Body.String()), "not running")
}

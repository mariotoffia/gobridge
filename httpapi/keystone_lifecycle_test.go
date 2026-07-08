package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/runtime"
)

// fakeBridgeController records StartBridge/StopBridge calls so the admin
// handlers can be exercised without a real supervisor. It models the clean
// pause/resume contract: both operations succeed unless an error is injected.
type fakeBridgeController struct {
	startCalls int
	stopCalls  int
	startErr   error
	stopErr    error
}

func (f *fakeBridgeController) StartBridge(context.Context) error {
	f.startCalls++
	return f.startErr
}

func (f *fakeBridgeController) StopBridge(context.Context) error {
	f.stopCalls++
	return f.stopErr
}

var _ BridgeController = (*fakeBridgeController)(nil)

// TestHandleLive_200AfterCleanStop reproduces CRITICAL 1/3 at the liveness
// probe: after a deliberate (clean) runtime Stop the runtime is NOT terminal,
// so /live must stay 200 and Kubernetes must NOT restart the container.
func TestHandleLive_200AfterCleanStop(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("keystone-live-after-stop"))
	s := New(rt, testConfig())

	ctx := context.Background()
	require.NoError(t, rt.Start(ctx))

	// Deliberate, clean stop — must not trip terminal.
	require.NoError(t, rt.Stop(ctx))
	require.False(t, rt.Terminal(), "a clean deliberate stop must not be terminal")

	rec := httptest.NewRecorder()
	s.handleLive(rec, httptest.NewRequest(http.MethodGet, "/live", nil))

	require.Equal(t, http.StatusOK, rec.Code,
		"/live must return 200 after a clean deliberate stop (CRITICAL 1/3)")
	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "alive", body["status"])
}

// TestAdmin_StopThenStart_NoPermanent409 reproduces CRITICAL 1: POST
// /bridge/stop followed by POST /bridge/start must both succeed when routed
// through the supervisor (BridgeController). Before the fix, stop was
// process-suicide and any later start hit a permanent single-use 409.
func TestAdmin_StopThenStart_NoPermanent409(t *testing.T) {
	ctrl := &fakeBridgeController{}
	cfg := testConfig()
	cfg.BridgeController = ctrl

	rt := runtime.New(runtime.WithInstanceID("keystone-stop-start"))
	s := New(rt, cfg)

	// Stop: clean pause routed through the controller → 200.
	stopRec := httptest.NewRecorder()
	s.handleStop(stopRec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/stop"))
	require.Equal(t, http.StatusOK, stopRec.Code, "/bridge/stop must return 200")
	require.Equal(t, 1, ctrl.stopCalls, "stop must route through the supervisor")

	// Start after stop: rebuild via the controller → 200, NOT a permanent 409.
	startRec := httptest.NewRecorder()
	s.handleStart(startRec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/start"))
	require.Equal(t, http.StatusOK, startRec.Code,
		"/bridge/start after /bridge/stop must succeed, never a permanent 409 (CRITICAL 1)")
	require.Equal(t, 1, ctrl.startCalls, "start must route through the supervisor")

	var body map[string]string
	require.NoError(t, json.NewDecoder(startRec.Body).Decode(&body))
	assert.Equal(t, "started", body["status"])
}

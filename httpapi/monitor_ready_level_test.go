package httpapi_test

import (
	"context"
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

// TestHandleReady_LegacyShape_Backwards compat ensures /ready (no query)
// keeps the {status, role} / {error} legacy contract even after the
// level= query parameter was added. Operators with existing tooling
// must keep working.
func TestHandleReady_LegacyShape_BackwardsCompat(t *testing.T) {
	rt := newBridgingRuntime(t, "legacy")
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: shared.NewSecret("key-0123456789abcdef")})

	rec := httptest.NewRecorder()
	s.MonitorMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "ready", body["status"])
	assert.Equal(t, "standalone", body["role"])
	_, hasLevel := body["level"]
	assert.False(t, hasLevel, "legacy response must NOT include level field")
}

// TestHandleReady_WithLevel_ReturnsStructuredResponse verifies that
// /ready?level=running returns the new structured response with level
// + requested fields.
func TestHandleReady_WithLevel_ReturnsStructuredResponse(t *testing.T) {
	rt := newBridgingRuntime(t, "structured")
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: shared.NewSecret("key-0123456789abcdef")})

	rec := httptest.NewRecorder()
	s.MonitorMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready?level=running", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "ready", body["status"])
	assert.Equal(t, "running", body["requested"])
	assert.Contains(t, []any{"running", "connected", "subscribed", "full"}, body["level"])
}

// TestHandleReady_LevelTooHigh_Returns503Structured verifies that asking
// for a level we have not achieved returns 503 with the structured body.
func TestHandleReady_LevelTooHigh_Returns503Structured(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("too-high"))
	// Don't Start — runtime is below LevelRunning.
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: shared.NewSecret("key-0123456789abcdef")})

	rec := httptest.NewRecorder()
	s.MonitorMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready?level=full", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "not_ready", body["status"])
	assert.Equal(t, "full", body["requested"])
	assert.Equal(t, "live", body["level"], "non-started runtime is at LevelLive")
}

// TestHandleReady_InvalidLevel_Returns400 verifies that an unknown
// level= value yields HTTP 400 Bad Request.
func TestHandleReady_InvalidLevel_Returns400(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("bad-level"))
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: shared.NewSecret("key-0123456789abcdef")})

	rec := httptest.NewRecorder()
	s.MonitorMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready?level=banana", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestRuntime_ReadinessLevel_EmptyRuntimeStopsAtRunning pins the two ends of
// the progression for a runtime that carries NOTHING — the start-empty state, a
// process booted without a usable configuration. Before Start it is only Live;
// after Start it is Running and goes no further. It must never reach LevelFull:
// "every route is ready" is vacuously true over zero routes, and a deployment
// that gates traffic on Full would otherwise open the gate for an instance
// through which not one message can pass.
func TestRuntime_ReadinessLevel_EmptyRuntimeStopsAtRunning(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("levels"))

	got := rt.ReadinessLevel(context.Background())
	assert.Equal(t, runtime.LevelLive, got, "before Start, level must be Live")

	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	got = rt.ReadinessLevel(context.Background())
	assert.Equal(t, runtime.LevelRunning, got,
		"a started runtime that carries no routes and no sessions bridges nothing and must stop at Running")
}

// TestHandleReady_EmptyRuntimeShedsTraffic proves the same contract through the
// probe an orchestrator actually calls: the legacy no-level /ready gates on
// LevelFull, so a bridge started with a missing or route-less config sheds
// traffic instead of advertising itself as a working member of the pool.
func TestHandleReady_EmptyRuntimeShedsTraffic(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("empty"))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	s := httpapi.New(rt, httpapi.Config{AdminAPIKey: shared.NewSecret("key-0123456789abcdef")})

	rec := httptest.NewRecorder()
	s.MonitorMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil))

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestParseReadinessLevel verifies all level strings round-trip.
func TestParseReadinessLevel(t *testing.T) {
	cases := []struct {
		in   string
		want runtime.ReadinessLevel
		ok   bool
	}{
		{"live", runtime.LevelLive, true},
		{"running", runtime.LevelRunning, true},
		{"connected", runtime.LevelConnected, true},
		{"subscribed", runtime.LevelSubscribed, true},
		{"full", runtime.LevelFull, true},
		{"FULL", runtime.LevelFull, true},
		{" full ", runtime.LevelFull, true},
		{"", runtime.LevelDown, false},
		{"banana", runtime.LevelDown, false},
	}
	for _, c := range cases {
		got, ok := runtime.ParseReadinessLevel(c.in)
		assert.Equal(t, c.ok, ok, "in=%q", c.in)
		if ok {
			assert.Equal(t, c.want, got, "in=%q", c.in)
			assert.Equal(t, c.in[:0]+got.String(), got.String(), "string round-trips")
		}
	}
}

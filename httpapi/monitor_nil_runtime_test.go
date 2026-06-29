package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// nilRuntimeMonitorServer returns a Server whose RuntimeProvider always
// returns nil, together with a monitor mux wired to its routes.
func nilRuntimeMonitorServer() (*Server, *http.ServeMux) {
	cfg := Config{
		AdminAddr:   ":0",
		MonitorAddr: ":0",
		AdminAPIKey: shared.NewSecret("test-admin-key-1234567890"),
		RuntimeProvider: func() ports.Runtime {
			return nil
		},
	}
	s := New(nil, cfg, WithServerLogger(nil))

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	return s, mux
}

// monitorNilReq builds a GET request, optionally adding the admin API key
// for endpoints that require authentication.
func monitorNilReq(path string, withAuth bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if withAuth {
		req.Header.Set("X-API-Key", "test-admin-key-1234567890")
	}
	return req
}

// TestMonitorHealth_NilRuntime_ReturnsUnavailable verifies that the health
// endpoint returns 503 with the HealthResponse shape (not ErrorResponse)
// when the RuntimeProvider yields nil.
func TestMonitorHealth_NilRuntime_ReturnsUnavailable(t *testing.T) {
	_, mux := nilRuntimeMonitorServer()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, monitorNilReq("/api/v1/monitor/health", false))

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "unavailable", body["status"],
		"health status should be 'unavailable'")
	assert.Equal(t, "", body["instance_id"],
		"instance_id should be empty string")
	assert.Equal(t, float64(0), body["routes"],
		"routes should be 0")

	// Verify it does NOT contain the error-response shape.
	_, hasError := body["error"]
	assert.False(t, hasError,
		"health response must use HealthResponse shape, not ErrorResponse")
}

// TestMonitorEndpoints_NilRuntime_Return503 verifies that the monitor
// endpoints (except /health and /live) return HTTP 503 with the standard
// ErrorResponse {"error":"runtime not available"} when the RuntimeProvider
// yields nil.
func TestMonitorEndpoints_NilRuntime_Return503(t *testing.T) {
	_, mux := nilRuntimeMonitorServer()

	tests := []struct {
		name     string
		path     string
		withAuth bool
	}{
		{
			name:     "GET /ready (unauthenticated)",
			path:     "/api/v1/monitor/ready",
			withAuth: false,
		},
		{
			name:     "GET /topology (authenticated)",
			path:     "/api/v1/monitor/topology",
			withAuth: true,
		},
		{
			name:     "GET /routes (authenticated)",
			path:     "/api/v1/monitor/routes",
			withAuth: true,
		},
		{
			name:     "GET /deephealth (authenticated)",
			path:     "/api/v1/monitor/deephealth",
			withAuth: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := monitorNilReq(tc.path, tc.withAuth)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
				"expected 503 for %s", tc.path)

			var body map[string]string
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&body),
				"response body should be valid JSON for %s", tc.path)
			assert.Equal(t, "runtime not available", body["error"],
				"expected error message for %s", tc.path)
		})
	}
}

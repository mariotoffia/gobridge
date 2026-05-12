package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// nilRuntimeAdminServer returns a Server whose RuntimeProvider always returns
// nil. This simulates the state before a runtime has been created (e.g. during
// supervisor startup).
func nilRuntimeAdminServer() (*Server, *http.ServeMux) {
	cfg := Config{
		AdminAddr:   ":0",
		MonitorAddr: ":0",
		AdminAPIKey: "test-admin-key-1234567890",
		RuntimeProvider: func() ports.Runtime {
			return nil
		},
	}
	s := New(nil, cfg, WithServerLogger(nil))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return s, mux
}

// adminNilReq builds a request with the admin API key header set.
func adminNilReq(method, path string, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-API-Key", "test-admin-key-1234567890")
	return req
}

// TestAdminHandlers_NilRuntime_Return503 verifies that every admin handler
// returns HTTP 503 with {"error":"runtime not available"} when the
// RuntimeProvider yields nil.
func TestAdminHandlers_NilRuntime_Return503(t *testing.T) {
	_, mux := nilRuntimeAdminServer()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "GET /bridge",
			method: http.MethodGet,
			path:   "/api/v1/admin/bridge",
		},
		{
			name:   "POST /bridge/start",
			method: http.MethodPost,
			path:   "/api/v1/admin/bridge/start",
		},
		{
			name:   "POST /bridge/stop",
			method: http.MethodPost,
			path:   "/api/v1/admin/bridge/stop",
		},
		{
			name:   "GET /routes",
			method: http.MethodGet,
			path:   "/api/v1/admin/routes",
		},
		{
			name:   "POST /routes/{routeID}/inject",
			method: http.MethodPost,
			path:   "/api/v1/admin/routes/test-route/inject",
			body:   `{"subject":"test","payload":""}`,
		},
		{
			name:   "GET /dlq",
			method: http.MethodGet,
			path:   "/api/v1/admin/dlq",
		},
		{
			name:   "GET /dlq/messages",
			method: http.MethodGet,
			path:   "/api/v1/admin/dlq/messages",
		},
		{
			name:   "POST /dlq/redrive",
			method: http.MethodPost,
			path:   "/api/v1/admin/dlq/redrive",
			body:   `{"ids":["msg-1"]}`,
		},
		{
			name:   "POST /dlq/delete",
			method: http.MethodPost,
			path:   "/api/v1/admin/dlq/delete",
			body:   `{"ids":["msg-1"]}`,
		},
		{
			name:   "POST /dlq/delete-by-filter",
			method: http.MethodPost,
			path:   "/api/v1/admin/dlq/delete-by-filter",
			body:   `{"route_id":"r1","confirm_delete_all":true}`,
		},
		{
			name:   "POST /dlq/purge",
			method: http.MethodPost,
			path:   "/api/v1/admin/dlq/purge",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := adminNilReq(tc.method, tc.path, tc.body)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
				"expected 503 for %s %s", tc.method, tc.path)

			var body map[string]string
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&body),
				"response body should be valid JSON for %s %s", tc.method, tc.path)
			assert.Equal(t, "runtime not available", body["error"],
				"expected error message for %s %s", tc.method, tc.path)
		})
	}
}

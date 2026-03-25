package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRuntime() *runtime.Runtime {
	return runtime.New(runtime.WithInstanceID("test-instance"))
}

func testConfig() Config {
	return Config{
		AdminAddr:   ":0",
		MonitorAddr: ":0",
		AdminAPIKey: "test-secret",
	}
}

// --- Config validation tests ---

func TestValidateConfig_AdminAPIKeyRequired(t *testing.T) {
	rt := testRuntime()
	cfg := DefaultConfig()
	s := New(rt, cfg)
	err := s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin API key is required")
}

func TestValidateConfig_CORSWildcardRejected(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "*"
	s := New(rt, cfg)
	err := s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wildcard CORS origin")
}

func TestValidateConfig_CORSWildcardInListRejected(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com, *"
	s := New(rt, cfg)
	err := s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wildcard CORS origin")
}

func TestValidateConfig_ExplicitCORSAllowed(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com,https://other.com"
	s := New(rt, cfg)
	err := s.validateConfig()
	require.NoError(t, err)
}

func TestValidateConfig_EmptyCORSAllowed(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)
	err := s.validateConfig()
	require.NoError(t, err)
}

// --- Admin auth tests ---

func TestAdminAuth_RequiredWhenKeySet(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	t.Run("missing key returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("wrong key returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("X-API-Key", "wrong")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("valid X-API-Key returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("X-API-Key", "test-secret")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("valid Bearer returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("Authorization", "Bearer test-secret")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- Monitor auth tests ---

func TestMonitorProbes_NoAuthRequired(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	for _, path := range []string{"/api/v1/monitor/health", "/api/v1/monitor/live", "/api/v1/monitor/ready"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"probe %s should not require auth", path)
		})
	}
}

func TestMonitorSensitive_RequiresAuth(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	sensitive := []string{
		"/api/v1/monitor/topology",
		"/api/v1/monitor/routes",
		"/api/v1/monitor/logs",
	}

	for _, path := range sensitive {
		t.Run("no auth "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusUnauthorized, rec.Code,
				"sensitive endpoint %s must require auth", path)
		})

		t.Run("with admin key "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-API-Key", "test-secret")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestMonitorSensitive_SeparateMonitorKey(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.MonitorAPIKey = "monitor-key"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	t.Run("admin key rejected for monitor", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
		req.Header.Set("X-API-Key", "test-secret")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("monitor key accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
		req.Header.Set("X-API-Key", "monitor-key")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- CORS tests ---

func TestCORS_DisabledByDefault(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
		"CORS should be disabled when CORSOrigins is empty")
}

func TestCORS_ExplicitOriginAllowed(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "https://example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Origin", rec.Header().Get("Vary"))
}

func TestCORS_UnlistedOriginRejected(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_PreflightReturns204(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "https://example.com", rec.Header().Get("Access-Control-Allow-Origin"))
}

// --- Handler tests ---

func TestHandleBridge(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "test-secret")
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
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/routes", nil)
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var routes []routeView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &routes))
	assert.Empty(t, routes)
}

func TestHandleHealth(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

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
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandleReady_NotRunning(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestMethodNotAllowed(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestDLQ_NoStore(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/dlq", nil)
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body["status"], "no DLQ")
}

// --- Audit logging tests ---

type recordingAuditLogger struct {
	mu     sync.Mutex
	events []ports.AuditEvent
}

func (r *recordingAuditLogger) Log(_ context.Context, ev ports.AuditEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingAuditLogger) Events() []ports.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]ports.AuditEvent, len(r.events))
	copy(cp, r.events)
	return cp
}

func TestAuditLogging_AdminCalls(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	audit := &recordingAuditLogger{}
	s := New(rt, cfg, WithAuditLogger(audit))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	events := audit.Events()
	require.Len(t, events, 1)
	assert.Equal(t, "bridge.status", events[0].Action)
	assert.Equal(t, "success", events[0].Outcome)
}

func TestAuditLogging_DLQPurge(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	audit := &recordingAuditLogger{}
	s := New(rt, cfg, WithAuditLogger(audit))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dlq/purge", nil)
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAuditLogging_DLQReplay(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	audit := &recordingAuditLogger{}
	s := New(rt, cfg, WithAuditLogger(audit))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	body := `{"ids":["id-1","id-2"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dlq/replay", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// --- Topology endpoint test ---

func TestHandleTopology(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "test-instance", body["instance_id"])
}

// --- SlogAuditLogger test ---

func TestSlogAuditLogger_LogsEvent(t *testing.T) {
	var buf strings.Builder
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	l := slog.New(h)
	audit := NewSlogAuditLogger(l)
	audit.Log(context.Background(), ports.AuditEvent{
		Timestamp:  time.Now(),
		Action:     "test.action",
		Actor:      "test-actor",
		Resource:   "test-resource",
		ResourceID: "r-123",
		Outcome:    "success",
		Detail:     map[string]any{"key": "value"},
	})
	assert.Contains(t, buf.String(), "test.action")
	assert.Contains(t, buf.String(), "test-actor")
	assert.Contains(t, buf.String(), "audit")
}

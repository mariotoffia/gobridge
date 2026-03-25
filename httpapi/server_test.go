package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
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

// --- Inject endpoint tests ---

type stubReceiver struct {
	ready chan struct{}
}

func newStubReceiver() *stubReceiver { return &stubReceiver{ready: make(chan struct{})} }

func (r *stubReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	close(r.ready)
	<-ctx.Done()
	return ctx.Err()
}

type stubSender struct {
	mu   sync.Mutex
	sent []*domain.Envelope
}

func (s *stubSender) Send(_ context.Context, env *domain.Envelope) error {
	s.mu.Lock()
	s.sent = append(s.sent, env.Clone())
	s.mu.Unlock()
	return nil
}

func (s *stubSender) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func injectRuntime(t *testing.T) (*runtime.Runtime, *stubSender) {
	t.Helper()
	sender := &stubSender{}
	rt := runtime.New(runtime.WithInstanceID("inject-http-test"))
	cfg := runtime.RouteConfig{
		ID: "test-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	err := rt.AddRoute(cfg, newStubReceiver(), sender, nil, nil)
	require.NoError(t, err)
	err = rt.Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	time.Sleep(50 * time.Millisecond)
	return rt, sender
}

func TestInject_RequiresAuth(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	body := `{"subject":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/any/inject", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInject_UnknownRoute(t *testing.T) {
	rt, _ := injectRuntime(t)
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	body := `{"subject":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/nonexistent/inject", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestInject_InvalidBody(t *testing.T) {
	rt, _ := injectRuntime(t)
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/test-route/inject", strings.NewReader("not json"))
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInject_InvalidBase64(t *testing.T) {
	rt, _ := injectRuntime(t)
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	body := `{"subject":"test","payload":"not-valid-base64!!!"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/test-route/inject", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.Contains(t, resp["error"], "base64")
}

func TestInject_HappyPath(t *testing.T) {
	rt, sender := injectRuntime(t)
	cfg := testConfig()
	audit := &recordingAuditLogger{}
	s := New(rt, cfg, WithAuditLogger(audit))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	payload := base64.StdEncoding.EncodeToString([]byte(`{"temp":22.5}`))
	body := `{"subject":"sensors/temp","payload":"` + payload + `","headers":{"source":"api"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/test-route/inject", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "injected", resp["status"])

	time.Sleep(50 * time.Millisecond)

	assert.Equal(t, 1, sender.sentCount(), "message should have been sent through the route")

	events := audit.Events()
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	assert.Equal(t, "route.inject", last.Action)
	assert.Equal(t, "success", last.Outcome)
	assert.Equal(t, "test-route", last.ResourceID)
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

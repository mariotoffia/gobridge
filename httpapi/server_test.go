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
		AdminAPIKey: "test-secret-key-0123456789",
	}
}

// --- statusRecorder (Flush / Unwrap) tests ---

type responseWriterSpy struct {
	header     http.Header
	flushCalls int
}

func (r *responseWriterSpy) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *responseWriterSpy) WriteHeader(int) {}

func (r *responseWriterSpy) Write(b []byte) (int, error) { return len(b), nil }

type flusherSpy struct {
	responseWriterSpy
}

func (f *flusherSpy) Flush() { f.flushCalls++ }

// Verifies statusRecorder.Flush delegates to the underlying http.Flusher.
func TestStatusRecorder_Flush_DelegatesToUnderlying(t *testing.T) {
	under := &flusherSpy{}
	rw := &statusRecorder{ResponseWriter: under, status: http.StatusOK}
	rw.Flush()
	assert.Equal(t, 1, under.flushCalls)
}

// Verifies statusRecorder.Flush is a no-op when the underlying writer is not an http.Flusher.
func TestStatusRecorder_Flush_NoopOnNonFlusher(t *testing.T) {
	under := &responseWriterSpy{}
	rw := &statusRecorder{ResponseWriter: under, status: http.StatusOK}
	assert.NotPanics(t, func() { rw.Flush() })
	assert.Equal(t, 0, under.flushCalls)
}

// Verifies statusRecorder.Unwrap returns the original ResponseWriter.
func TestStatusRecorder_Unwrap_ReturnsOriginal(t *testing.T) {
	under := &responseWriterSpy{}
	rw := &statusRecorder{ResponseWriter: under, status: http.StatusOK}
	assert.Same(t, under, rw.Unwrap())
}

// --- Config validation tests ---

// Verifies Start fails when the admin API key is not configured.
func TestValidateConfig_AdminAPIKeyRequired(t *testing.T) {
	rt := testRuntime()
	cfg := DefaultConfig()
	s := New(rt, cfg)
	err := s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin API key is required")
}

// Verifies Start rejects a lone wildcard CORS origin.
func TestValidateConfig_CORSWildcardRejected(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "*"
	s := New(rt, cfg)
	err := s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wildcard CORS origin")
}

// Verifies Start rejects wildcard CORS when it appears in a comma-separated list.
func TestValidateConfig_CORSWildcardInListRejected(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com, *"
	s := New(rt, cfg)
	err := s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wildcard CORS origin")
}

// Verifies validateConfig accepts explicit comma-separated CORS origins.
func TestValidateConfig_ExplicitCORSAllowed(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com,https://other.com"
	s := New(rt, cfg)
	err := s.validateConfig()
	require.NoError(t, err)
}

// Verifies validateConfig allows empty CORS configuration (disabled).
func TestValidateConfig_EmptyCORSAllowed(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)
	err := s.validateConfig()
	require.NoError(t, err)
}

// Verifies validateConfig rejects an admin API key shorter than minAPIKeyLen.
func TestValidateConfig_AdminAPIKeyTooShort(t *testing.T) {
	rt := testRuntime()
	cfg := DefaultConfig()
	cfg.AdminAPIKey = "short"
	s := New(rt, cfg)
	err := s.validateConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least")
}

// Verifies validateConfig rejects a monitor API key shorter than minAPIKeyLen.
func TestValidateConfig_MonitorAPIKeyTooShort(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.MonitorAPIKey = "short"
	s := New(rt, cfg)
	err := s.validateConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "monitor API key")
}

// Verifies security headers are set on responses through the wrap middleware.
func TestSecurityHeaders_SetOnResponse(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
}

// --- Admin auth tests ---

// Verifies admin routes require authentication when an API key is set: missing or wrong keys yield 401; valid X-API-Key or Bearer succeeds.
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
		req.Header.Set("X-API-Key", "test-secret-key-0123456789")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("valid Bearer returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("Authorization", "Bearer test-secret-key-0123456789")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- Monitor auth tests ---

// Verifies health, live, and ready probe endpoints do not require authentication.
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

// Verifies topology, routes, and logs monitor endpoints require auth without a key and return 200 with the admin API key.
func TestMonitorSensitive_RequiresAuth(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	sensitive := []string{
		"/api/v1/monitor/topology",
		"/api/v1/monitor/routes",
		"/api/v1/monitor/deephealth",
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
			req.Header.Set("X-API-Key", "test-secret-key-0123456789")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			switch path {
			case "/api/v1/monitor/logs":
				assert.Equal(t, http.StatusNotImplemented, rec.Code)
			case "/api/v1/monitor/deephealth":
				// Deep health returns 503 when runtime is not running.
				assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			default:
				assert.Equal(t, http.StatusOK, rec.Code)
			}
		})
	}
}

// Verifies a dedicated MonitorAPIKey is enforced: admin key is rejected, monitor key is accepted for sensitive monitor routes.
func TestMonitorSensitive_SeparateMonitorKey(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.MonitorAPIKey = "monitor-key-0123456789ab"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	t.Run("admin key rejected for monitor", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
		req.Header.Set("X-API-Key", "test-secret-key-0123456789")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("monitor key accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
		req.Header.Set("X-API-Key", "monitor-key-0123456789ab")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- CORS tests ---

// Verifies no Access-Control-Allow-Origin is set when CORS origins are not configured.
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

// Verifies an allowed Origin receives Access-Control-Allow-Origin and Vary: Origin.
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

// Verifies a request Origin not in the allowlist does not get CORS reflection headers.
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

// Verifies OPTIONS preflight for an allowed origin returns 204 No Content with CORS headers.
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

// Verifies OPTIONS preflight for a disallowed origin returns 403.
func TestCORS_PreflightDisallowedOriginReturns403(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

// --- Handler tests ---

// Verifies GET /api/v1/admin/bridge returns instance metadata and running=false when authenticated.
func TestHandleBridge(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "test-instance", body["instance_id"])
	assert.Equal(t, false, body["running"])
}

// Verifies GET /api/v1/admin/routes returns an empty JSON array when no routes exist.
func TestHandleRoutes_Empty(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/routes", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var routes []routeView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &routes))
	assert.Empty(t, routes)
}

// Verifies GET /api/v1/monitor/health reports not_running and 503 when the bridge is not running.
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

// Verifies GET /api/v1/monitor/live returns 200 for liveness.
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

// Verifies GET /api/v1/monitor/ready returns 503 when the bridge is not ready.
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

// Verifies unsupported HTTP methods on admin routes return 405 Method Not Allowed.
func TestMethodNotAllowed(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// Verifies GET /api/v1/admin/dlq succeeds with a status indicating no DLQ when no store is configured.
func TestDLQ_NoStore(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/dlq", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
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

// Verifies a successful admin bridge status call emits one audit event with expected action and outcome.
func TestAuditLogging_AdminCalls(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	audit := &recordingAuditLogger{}
	s := New(rt, cfg, WithAuditLogger(audit))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	events := audit.Events()
	require.Len(t, events, 1)
	assert.Equal(t, "bridge.status", events[0].Action)
	assert.Equal(t, "success", events[0].Outcome)
}

// Verifies POST /api/v1/admin/dlq/purge returns 404 when no DLQ store is configured.
func TestAuditLogging_DLQPurge(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	audit := &recordingAuditLogger{}
	s := New(rt, cfg, WithAuditLogger(audit))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dlq/purge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// Verifies POST /api/v1/admin/dlq/replay returns 404 when no DLQ store is configured.
func TestAuditLogging_DLQReplay(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	audit := &recordingAuditLogger{}
	s := New(rt, cfg, WithAuditLogger(audit))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	body := `{"ids":["id-1","id-2"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dlq/replay", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
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

// Verifies the inject endpoint returns 401 without authentication.
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

// Verifies inject returns 404 for a route ID that does not exist.
func TestInject_UnknownRoute(t *testing.T) {
	rt, _ := injectRuntime(t)
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	body := `{"subject":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/nonexistent/inject", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// Verifies inject rejects a non-JSON body with 400 Bad Request.
func TestInject_InvalidBody(t *testing.T) {
	rt, _ := injectRuntime(t)
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/test-route/inject", strings.NewReader("not json"))
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Verifies inject rejects invalid base64 payload with an error mentioning base64.
func TestInject_InvalidBase64(t *testing.T) {
	rt, _ := injectRuntime(t)
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	body := `{"subject":"test","payload":"not-valid-base64!!!"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/test-route/inject", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.Contains(t, resp["error"], "base64")
}

// Verifies a valid inject request returns 200, delivers through the route sender, and records a successful route.inject audit event.
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
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
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

// Verifies GET /api/v1/monitor/topology returns instance_id when authenticated.
func TestHandleTopology(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "test-instance", body["instance_id"])
}

// --- SlogAuditLogger test ---

// TestServer_DoubleStart_ReturnsError validates that calling Start twice
// returns an error on the second call.
func TestServer_DoubleStart_ReturnsError(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	s := New(rt, cfg)

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	err := s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already running")
}

// TestSanitizePropagatedID_Truncation validates that sanitizePropagatedID
// truncates strings longer than 256 bytes.
func TestSanitizePropagatedID_Truncation(t *testing.T) {
	tests := []struct {
		name    string
		inputLen int
		wantLen  int
	}{
		{name: "empty", inputLen: 0, wantLen: 0},
		{name: "short", inputLen: 100, wantLen: 100},
		{name: "exactly_256", inputLen: 256, wantLen: 256},
		{name: "over_256", inputLen: 257, wantLen: 256},
		{name: "much_longer", inputLen: 1024, wantLen: 256},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Repeat("x", tt.inputLen)
			got := sanitizePropagatedID(input)
			assert.Len(t, got, tt.wantLen)
			if tt.inputLen <= 256 {
				assert.Equal(t, input, got)
			}
		})
	}
}

// Verifies SlogAuditLogger writes audit fields including action, actor, and audit marker to the slog output.
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

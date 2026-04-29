package httpapi

import (
	"bytes"
	"context"
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

// ---------------------------------------------------------------------------
// BUG-6: CORS preflight returns 403 for disallowed origins
// ---------------------------------------------------------------------------

// Verifies OPTIONS with no Origin header returns 403 when CORS is enabled.
// Without an origin, the CORS middleware cannot determine if the request is
// allowed, so it must reject the preflight.
func TestBug6_CORS_PreflightNoOriginHeader(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com"
	s := New(rt, cfg)

	handler := s.wrap(http.NewServeMux())

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitor/live", nil)
	// No Origin header set.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"OPTIONS without Origin should return 403 when CORS is enabled")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
		"no CORS headers should be set without Origin")
}

// Verifies OPTIONS with an empty string Origin header returns 403.
func TestBug6_CORS_PreflightEmptyOriginHeader(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com"
	s := New(rt, cfg)

	handler := s.wrap(http.NewServeMux())

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"OPTIONS with empty Origin should return 403")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

// Verifies OPTIONS when CORSOrigins is empty (CORS disabled) does not engage
// the CORS middleware at all. The request should pass through to the underlying
// handler or default 200, not 403.
func TestBug6_CORS_PreflightCORSDisabled(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	// CORSOrigins is empty -- CORS is disabled.
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// When CORS middleware is not applied, OPTIONS goes to the handler which
	// will respond (Go's default mux returns 200 for OPTIONS).
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"OPTIONS should not return 403 when CORS is disabled")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
		"no CORS headers should be set when CORS is disabled")
}

// Verifies a GET request from a disallowed origin still serves the response
// (CORS is browser-advisory), but without CORS reflection headers.
func TestBug6_CORS_GetDisallowedOriginStillServed(t *testing.T) {
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

	assert.Equal(t, http.StatusOK, rec.Code,
		"GET with disallowed origin should still serve the response")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
		"disallowed origin should not get CORS reflection headers")
}

// Verifies that the second origin in a comma-separated CORSOrigins list is
// accepted for preflight.
func TestBug6_CORS_MultipleOriginsSecondAllowed(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://first.com,https://second.com"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://second.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code,
		"second origin should be allowed")
	assert.Equal(t, "https://second.com",
		rec.Header().Get("Access-Control-Allow-Origin"))
}

// Verifies origins with trailing slash do not match (exact match semantics).
func TestBug6_CORS_TrailingSlashMismatch(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://example.com/")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"origin with trailing slash should not match exact origin without it")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

// Verifies origin matching is case-sensitive ("https://Example.Com" does not
// match "https://example.com").
func TestBug6_CORS_CaseSensitiveOrigin(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://example.com"
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitor/live", nil)
	req.Header.Set("Origin", "https://Example.Com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"origin matching should be case-sensitive")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

// ---------------------------------------------------------------------------
// BUG-7: writeJSON logs encoding errors
// ---------------------------------------------------------------------------

// captureAndRestoreDefaultLogger replaces the default slog logger with one
// writing to the returned buffer, and returns a cleanup function.
func captureAndRestoreDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := slog.Default()
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

// Verifies writeJSON with a normal encodable value produces no slog error output.
func TestBug7_WriteJSON_NormalEncoding_NoLogOutput(t *testing.T) {
	buf := captureAndRestoreDefaultLogger(t)

	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"key": "value"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "value", body["key"])

	assert.Empty(t, buf.String(),
		"no slog error should be emitted for normal encoding")
}

// Verifies writeJSON with an unencodable value (function) emits slog.Error.
func TestBug7_WriteJSON_UnencodableValue_LogsSlogError(t *testing.T) {
	buf := captureAndRestoreDefaultLogger(t)

	rec := httptest.NewRecorder()
	unencodable := map[string]any{"fn": func() {}}
	writeJSON(rec, http.StatusOK, unencodable)

	assert.Equal(t, http.StatusOK, rec.Code,
		"status code should still be set even when encoding fails")

	logOutput := buf.String()
	assert.Contains(t, logOutput, "failed to encode JSON response",
		"slog.Error should be called with the encoding failure message")
	assert.Contains(t, strings.ToLower(logOutput), "error",
		"log output should contain error level or field")
}

// Verifies writeJSON with nil data does not attempt encoding and emits no log.
func TestBug7_WriteJSON_NilData_NoEncoding(t *testing.T) {
	buf := captureAndRestoreDefaultLogger(t)

	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusNoContent, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"),
		"Content-Type should be set before encoding even for nil data")
	assert.Empty(t, rec.Body.String(),
		"no body should be written for nil data")
	assert.Empty(t, buf.String(),
		"no slog error should be emitted for nil data")
}

// Verifies status code is set correctly even when encoding fails.
func TestBug7_WriteJSON_StatusCodeSetBeforeEncoding(t *testing.T) {
	captureAndRestoreDefaultLogger(t)

	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]any{"ch": make(chan int)})

	assert.Equal(t, http.StatusCreated, rec.Code,
		"status code should reflect what was passed, regardless of encoding")
}

// Verifies Content-Type header is set before encoding is attempted.
func TestBug7_WriteJSON_ContentTypeSetBeforeEncode(t *testing.T) {
	captureAndRestoreDefaultLogger(t)

	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"ok": "yes"})

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

// ---------------------------------------------------------------------------
// BUG-8: UTC consistency in DLQ purge
// ---------------------------------------------------------------------------

// captureDLQStore is a mock DLQ store that captures the time argument from
// Purge() so tests can verify it is UTC.
type captureDLQStore struct {
	mu          sync.Mutex
	purgeCalled bool
	purgeTime   time.Time
	purgeN      int
	purgeErr    error

	listEntries []domain.DLQEntry
}

func (s *captureDLQStore) Write(_ context.Context, _ domain.DLQEntry) error {
	return nil
}

func (s *captureDLQStore) List(_ context.Context, _ domain.DLQFilter) ([]domain.DLQEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listEntries != nil {
		return s.listEntries, nil
	}
	return []domain.DLQEntry{}, nil
}

func (s *captureDLQStore) Get(_ context.Context, _ string) (domain.DLQEntry, error) {
	return domain.DLQEntry{}, domain.ErrNotFound
}

func (s *captureDLQStore) Delete(_ context.Context, _ []string) (int, error) { return 0, nil }

func (s *captureDLQStore) DeleteByFilter(_ context.Context, _ domain.DLQFilter) (int, error) {
	return 0, nil
}

func (s *captureDLQStore) Purge(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeCalled = true
	s.purgeTime = before
	return s.purgeN, s.purgeErr
}

func (s *captureDLQStore) getPurgeTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeTime
}

func (s *captureDLQStore) wasPurgeCalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeCalled
}

// Verify interface satisfaction.
var _ ports.DLQStore = (*captureDLQStore)(nil)

// Verifies that the DLQ purge handler passes a UTC time to store.Purge().
func TestBug8_DLQPurge_PassesUTCToStore(t *testing.T) {
	store := &captureDLQStore{purgeN: 5}

	rt := runtime.New(
		runtime.WithInstanceID("test-utc-purge"),
		runtime.WithDLQStore(store),
	)
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dlq/purge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 5, body["purged"])

	require.True(t, store.wasPurgeCalled(), "Purge should have been called")
	purgeTime := store.getPurgeTime()
	assert.Equal(t, time.UTC, purgeTime.Location(),
		"Purge should receive UTC time, got location: %v", purgeTime.Location())
}

// Verifies that the audit log timestamp for DLQ purge uses UTC.
func TestBug8_DLQPurge_AuditLogUsesUTC(t *testing.T) {
	store := &captureDLQStore{purgeN: 3}

	rt := runtime.New(
		runtime.WithInstanceID("test-utc-audit"),
		runtime.WithDLQStore(store),
	)
	cfg := testConfig()
	audit := &recordingAuditLogger{}
	s := New(rt, cfg, WithAuditLogger(audit))

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dlq/purge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	events := audit.Events()
	require.NotEmpty(t, events, "at least one audit event should be emitted")

	purgeEvent := events[len(events)-1]
	assert.Equal(t, "dlq.purge", purgeEvent.Action)
	assert.Equal(t, "success", purgeEvent.Outcome)
	assert.Equal(t, time.UTC, purgeEvent.Timestamp.Location(),
		"audit event timestamp should be UTC")
}

// Verifies the Purge time argument is UTC by checking Location == time.UTC
// and that the time is recent (within 5 seconds of now UTC).
func TestBug8_DLQPurge_TimeIsRecentUTC(t *testing.T) {
	store := &captureDLQStore{purgeN: 0}

	rt := runtime.New(
		runtime.WithInstanceID("test-utc-recent"),
		runtime.WithDLQStore(store),
	)
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	beforeCall := time.Now().UTC()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dlq/purge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	afterCall := time.Now().UTC()

	require.True(t, store.wasPurgeCalled())

	purgeTime := store.getPurgeTime()
	assert.Equal(t, time.UTC, purgeTime.Location(),
		"purge time Location must be time.UTC")
	assert.False(t, purgeTime.Before(beforeCall),
		"purge time should not be before the call was made")
	assert.False(t, purgeTime.After(afterCall),
		"purge time should not be after the call completed")
}

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecoverMW_PanicsReturn500 validates that the recovery middleware catches
// panics from inner handlers and returns a 500 response instead of crashing.
func TestRecoverMW_PanicsReturn500(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-panic"))
	s := New(rt, testConfig(), WithServerLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))

	panicker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("unexpected failure")
	})

	handler := s.recoverMW(panicker)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "internal error", body["error"])
}

// TestRequestLogMW_LogsRequestDetails validates that the request logging
// middleware writes structured log output including method, path, status, and duration.
func TestRequestLogMW_LogsRequestDetails(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	rt := runtime.New(runtime.WithInstanceID("test-reqlog"))
	s := New(rt, testConfig(), WithServerLogger(logger))

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := s.requestLogMW(inner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	handler.ServeHTTP(rec, req)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "http request")
	assert.Contains(t, logOutput, "GET")
	assert.Contains(t, logOutput, "/api/test")
	assert.Contains(t, logOutput, "status")
	assert.Contains(t, logOutput, "duration_ms")
}

// TestServer_Stop_GracefulShutdown validates that Stop returns nil after
// a successful Start and the server stops cleanly.
func TestServer_Stop_GracefulShutdown(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-stop-graceful"))
	cfg := testConfig()
	cfg.AdminAddr = ":0"
	cfg.MonitorAddr = ":0"
	s := New(rt, cfg)

	require.NoError(t, s.Start(context.Background()))

	err := s.Stop(context.Background())
	assert.NoError(t, err)
}

// TestServer_Stop_NotRunning validates that Stop on a never-started server
// returns nil (no-op).
func TestServer_Stop_NotRunning(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-stop-noop"))
	s := New(rt, testConfig())

	err := s.Stop(context.Background())
	assert.NoError(t, err)
}

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/require"
)

func TestIntegration_ObservedConfigLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback HTTP lifecycle")
	}
	requireBlankBuild(t)
	t.Setenv(adminAPIKeyEnv, "")
	t.Setenv(monitorAPIKeyEnv, "")
	clk := clocktest.New()
	ctx, cancel := context.WithCancel(t.Context())
	output := &controlPlaneLog{ready: make(chan [2]string, 1)}
	logger := slog.New(slog.NewJSONHandler(output, nil))
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	done := make(chan error, 1)
	control := ports.HTTPConfig{AdminAddr: "127.0.0.1:0", MonitorAddr: "127.0.0.1:0", AdminAPIKey: shared.NewSecret("initial-control-secret")}
	go func() {
		done <- runObservedConfig(ctx, path, t.TempDir(), ports.NewRegistry(), control, "bridge:\n  id: embedded\n", logger, clk)
	}()
	t.Cleanup(func() { cancel(); require.NoError(t, wait.RequireReceive(t, done, 3*time.Second)) })
	urls := wait.RequireReceive(t, output.ready, time.Second)
	client := &http.Client{Timeout: time.Second}
	status := func(url string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", "initial-control-secret")
		resp, err := client.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, status(urls[1]+"/api/v1/monitor/live"))
	require.Equal(t, http.StatusServiceUnavailable, status(urls[1]+"/api/v1/monitor/ready"))
	advanceUntil := func(description string, condition func() bool) {
		wait.Until(t, 3*time.Second, description, func() bool { clk.Advance(time.Second); return condition() })
	}
	advanceUntil("embedded configuration activates", func() bool { return status(urls[0]+"/api/v1/admin/config") == http.StatusOK })
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	advanceUntil("confirmed deletion reaches real idle", func() bool { return status(urls[0]+"/api/v1/admin/config") == http.StatusServiceUnavailable })
	require.Equal(t, http.StatusOK, status(urls[1]+"/api/v1/monitor/live"))
	clk.Advance(10 * time.Second)
	require.ErrorIs(t, func() error { _, err := os.Stat(path); return err }(), os.ErrNotExist)
	require.NoError(t, os.WriteFile(path, saved, 0o600))
	advanceUntil("identical restored config rebuilds", func() bool { return status(urls[1]+"/api/v1/monitor/ready?level=running") == http.StatusOK })
}

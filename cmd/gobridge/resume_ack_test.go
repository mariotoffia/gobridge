package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/require"
)

func TestIntegration_PauseChangeResumeAcknowledgesCurrentObservation(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback command lifecycle")
	}
	requireBlankBuild(t)
	t.Setenv(adminAPIKeyEnv, "")
	t.Setenv(monitorAPIKeyEnv, "")
	ctx, cancel := context.WithCancel(t.Context())
	clk := clocktest.New()
	path, credentials := filepath.Join(t.TempDir(), "bridge.yaml"), t.TempDir()
	cfg := &ports.BridgeConfig{Version: 1, Bridge: ports.BridgeSettings{ID: "resume", LogLevel: "info"}}
	require.NoError(t, parser.WriteFile(path, cfg))
	output := &controlPlaneLog{ready: make(chan [2]string, 1)}
	control := ports.HTTPConfig{AdminAddr: "127.0.0.1:0", MonitorAddr: "127.0.0.1:0", AdminAPIKey: shared.NewSecret("resume-control-secret")}
	done := make(chan error, 1)
	go func() {
		done <- runObservedConfig(ctx, path, credentials, ports.NewRegistry(), control, "", slog.New(slog.NewJSONHandler(output, nil)), clk)
	}()
	t.Cleanup(func() { cancel(); require.NoError(t, wait.RequireReceive(t, done, time.Second)) })
	urls := wait.RequireReceive(t, output.ready, time.Second)
	call := func(method, url string) (int, []byte) {
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", "resume-control-secret")
		resp, err := (&http.Client{Timeout: time.Second}).Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, data
	}
	health := func() httpapi.ConfigWatchHealth {
		_, data := call(http.MethodGet, urls[1]+"/api/v1/monitor/deephealth")
		var decoded struct {
			ConfigWatch httpapi.ConfigWatchHealth `json:"config_watch"`
		}
		require.NoError(t, json.Unmarshal(data, &decoded))
		return decoded.ConfigWatch
	}
	advanceUntil := func(description string, condition func() bool) {
		wait.Until(t, time.Second, description, func() bool { clk.Advance(time.Second); return condition() })
	}
	advanceUntil("initial observation acknowledged", func() bool { v := health().RunningVersion; return v != nil && *v == 1 })
	code, _ := call(http.MethodPost, urls[0]+"/api/v1/admin/bridge/stop")
	require.Equal(t, http.StatusOK, code)
	cfg.Version, cfg.Bridge.LogLevel = 2, "debug"
	require.NoError(t, parser.WriteFile(path, cfg))
	advanceUntil("paused supervisor records the new observation", func() bool {
		_, data := call(http.MethodGet, urls[0]+"/api/v1/admin/config")
		var decoded struct {
			Config ports.BridgeConfig `json:"config"`
		}
		require.NoError(t, json.Unmarshal(data, &decoded))
		return decoded.Config.Version == 2
	})
	require.True(t, health().ReconfigurePending)
	code, _ = call(http.MethodPost, urls[0]+"/api/v1/admin/bridge/start")
	require.Equal(t, http.StatusOK, code)
	advanceUntil("resumed observation is confirmed running", func() bool {
		state := health()
		return state.RunningVersion != nil && *state.RunningVersion == 2 && !state.ReconfigurePending
	})
}

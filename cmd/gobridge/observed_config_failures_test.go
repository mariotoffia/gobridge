package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration_BlockedInitializerDoesNotBlockShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback command lifecycle")
	}
	requireBlankBuild(t)
	t.Setenv(adminAPIKeyEnv, "")
	ctx, cancel := context.WithCancel(t.Context())
	clk := clocktest.New()
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	hooks := observedConfigHooks{initialize: func(ctx context.Context) error { calls.Add(1); entered <- ctx; <-release; return nil }}
	path, credentials := filepath.Join(t.TempDir(), "bridge.yaml"), t.TempDir()
	output := &controlPlaneLog{ready: make(chan [2]string, 1)}
	control := ports.HTTPConfig{AdminAddr: "127.0.0.1:0", MonitorAddr: "127.0.0.1:0", AdminAPIKey: shared.NewSecret("initial-control-secret")}
	done := make(chan error, 1)
	go func() {
		done <- runObservedConfig(ctx, path, credentials, ports.NewRegistry(), control, "bridge: {id: blocked}", slog.New(slog.NewJSONHandler(output, nil)), clk, hooks)
	}()
	finished := false
	t.Cleanup(func() {
		unblock()
		cancel()
		if !finished {
			require.NoError(t, wait.RequireReceive(t, done, time.Second))
		}
	})
	wait.RequireReceive(t, output.ready, time.Second)
	wait.Until(t, time.Second, "initializer started", func() bool { clk.Advance(time.Second); return calls.Load() == 1 })
	attempt := wait.RequireReceive(t, entered, time.Second)
	require.Equal(t, int32(1), wait.StableFor(t, func() int32 { clk.Advance(time.Second); return calls.Load() }, 30*time.Millisecond, 100*time.Millisecond))
	cancel()
	err := wait.RequireReceive(t, done, 100*time.Millisecond)
	finished = true
	require.NoError(t, err)
	require.ErrorIs(t, attempt.Err(), context.Canceled)
	require.Equal(t, int32(1), calls.Load())
}

func TestIntegration_AsyncStartupFailureWaitsForRetryTick(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback command lifecycle")
	}
	requireBlankBuild(t)
	t.Setenv(adminAPIKeyEnv, "")
	ctx, cancel := context.WithCancel(t.Context())
	clk := clocktest.New()
	path, credentials := filepath.Join(t.TempDir(), "bridge.yaml"), t.TempDir()
	require.NoError(t, parser.WriteFile(path, &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "retry"}}))
	var starts atomic.Int32
	second, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	failure := errors.New("persistent startup failure")
	hooks := observedConfigHooks{start: func(cfg *ports.BridgeConfig, _ uint64) (*configSession, error) {
		if starts.Add(1) == 2 {
			close(second)
			<-release
		}
		session := &configSession{initial: cfg, sup: bridge.NewSupervisor(), cancel: func() {}, done: make(chan error, 1)}
		session.done <- failure
		return session, nil
	}}
	output := &controlPlaneLog{ready: make(chan [2]string, 1)}
	control := ports.HTTPConfig{AdminAddr: "127.0.0.1:0", MonitorAddr: "127.0.0.1:0", AdminAPIKey: shared.NewSecret("initial-control-secret")}
	done := make(chan error, 1)
	go func() {
		done <- runObservedConfig(ctx, path, credentials, ports.NewRegistry(), control, "", slog.New(slog.NewJSONHandler(output, nil)), clk, hooks)
	}()
	t.Cleanup(func() { unblock(); cancel(); require.NoError(t, wait.RequireReceive(t, done, time.Second)) })
	urls := wait.RequireReceive(t, output.ready, time.Second)
	wait.Until(t, time.Second, "first supervisor attempt", func() bool { return starts.Load() >= 1 })
	assert.False(t, wait.Poll(50*time.Millisecond, func() bool { return starts.Load() > 1 }), "no second attempt is permitted without a retry tick")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urls[1]+"/api/v1/monitor/deephealth", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", "initial-control-secret")
	response, err := (&http.Client{Timeout: time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	var health struct {
		ConfigWatch struct {
			LastApplyError string `json:"last_apply_error"`
		} `json:"config_watch"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&health))
	assert.Contains(t, health.ConfigWatch.LastApplyError, failure.Error(), "report the actual startup failure")
	clk.Advance(time.Second)
	wait.RequireClosed(t, second, time.Second)
	unblock()
	require.Equal(t, int32(2), wait.StableFor(t, starts.Load, 30*time.Millisecond, 100*time.Millisecond))
}

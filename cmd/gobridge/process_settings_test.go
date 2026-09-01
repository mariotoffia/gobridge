package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// captureLogger returns a logger writing structured text into buf so a test can
// assert on what an operator is actually told.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestStartEmpty_DoesNotAdvertiseAnAbsentAdminAPI proves the missing-config
// warning stays truthful. The start-empty config carries no `http` block, and
// this composition root creates its HTTP listeners once from the boot config,
// so a missing file means there is no admin API and no probe port at all.
// Telling the operator to "push a config through the admin config API" sends
// them at an endpoint that does not exist.
func TestStartEmpty_DoesNotAdvertiseAnAbsentAdminAPI(t *testing.T) {
	var buf bytes.Buffer
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	loader := configLoader(fileconfig.NewSource(path, ports.NewRegistry()), path, true, captureLogger(&buf))

	cfg, err := loader.Load(context.Background())
	require.NoError(t, err)
	require.Nil(t, cfg.HTTP, "precondition: the start-empty config defines no HTTP block")

	msg := buf.String()
	require.NotEmpty(t, msg, "a missing config file must be warned about")
	assert.NotContains(t, strings.ToLower(msg), "admin config api",
		"start-empty must not advertise an admin API this process will not serve")
	assert.Contains(t, strings.ToLower(msg), "restart",
		"start-empty must tell the operator the recovery path that actually works: create the file and restart")
}

// TestStartEmpty_CanBeRefused proves a deployment that must never bridge
// nothing can turn the fallback off: with start-empty disabled a missing config
// file stays a fatal load error instead of silently booting a route-less bridge.
func TestStartEmpty_CanBeRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	loader := configLoader(fileconfig.NewSource(path, ports.NewRegistry()), path, false, discardLogger())

	_, err := loader.Load(context.Background())

	require.Error(t, err, "with start-empty refused a missing config file must fail the process")
	assert.ErrorIs(t, err, shared.ErrNotFound)
}

// TestCurrentShutdownTimeout_ReadsTheRunningConfig proves the process shutdown
// budget follows a reloaded config. Pinning it to the boot config means an
// operator who raises bridge.shutdown_timeout through a reload keeps the old,
// too-short budget for the HTTP drain and the final supervisor wait — the
// process is cut off mid-drain by a value it no longer runs.
func TestCurrentShutdownTimeout_ReadsTheRunningConfig(t *testing.T) {
	boot := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b", ShutdownTimeout: "30s"}}
	running := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b", ShutdownTimeout: "90s"}}

	got := currentShutdownTimeout(func() *ports.BridgeConfig { return running }, boot)

	assert.Equal(t, 90*time.Second, got)
}

// TestCurrentShutdownTimeout_FallsBackToBootConfig proves the boot config still
// answers when the supervisor has nothing active to report (it never built a
// runtime, or it is wedged).
func TestCurrentShutdownTimeout_FallsBackToBootConfig(t *testing.T) {
	boot := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b", ShutdownTimeout: "45s"}}

	got := currentShutdownTimeout(func() *ports.BridgeConfig { return nil }, boot)

	assert.Equal(t, 45*time.Second, got)
}

// TestWatchErrorReason_NamesEveryFailedLayerDeterministically proves a config
// watcher that can no longer observe changes is reported to operators with the
// layer that failed and why, in a stable order (map iteration order is not).
func TestWatchErrorReason_NamesEveryFailedLayerDeterministically(t *testing.T) {
	errs := map[string]error{
		"secrets": errors.New("permission denied"),
		"file":    errors.New("inotify watch limit reached"),
	}

	got := watchErrorReason(errs)

	assert.Equal(t,
		"config watch degraded: file: inotify watch limit reached; secrets: permission denied",
		got)
}

// TestWatchErrorReason_EmptyWhenEveryWatcherIsHealthy proves the projection
// stays silent while nothing is wrong, so "degraded" keeps meaning something.
func TestWatchErrorReason_EmptyWhenEveryWatcherIsHealthy(t *testing.T) {
	assert.Empty(t, watchErrorReason(nil))
	assert.Empty(t, watchErrorReason(map[string]error{}))
}

// TestHTTPTopologyRestartReason_DetectsADivergentListenerBlock proves an
// operator who reloads a changed `http` block is told the change is inert. This
// root binds its admin and monitor listeners once, from the boot config, so a
// reloaded address, TLS pair or CORS origin is accepted and durably stored but
// never applied — silently, until now.
func TestHTTPTopologyRestartReason_DetectsADivergentListenerBlock(t *testing.T) {
	boot := &ports.HTTPConfig{AdminAddr: ":8080", MonitorAddr: ":8081"}
	running := &ports.HTTPConfig{AdminAddr: ":9090", MonitorAddr: ":8081"}

	assert.NotEmpty(t, httpTopologyRestartReason(boot, running),
		"a changed listener address must be reported as restart-required")
	assert.Empty(t, httpTopologyRestartReason(boot, &ports.HTTPConfig{AdminAddr: ":8080", MonitorAddr: ":8081"}),
		"an unchanged HTTP block must not raise a restart-required signal")
	assert.Empty(t, httpTopologyRestartReason(nil, nil),
		"a deployment that never configured HTTP has no topology to diverge")
}

// TestHTTPTopologyRestartReason_ComparesEffectiveAddresses proves the signal
// compares what the listeners actually bound, not the raw strings. An omitted
// admin_addr binds the same port as an explicit ":8080", so writing the default
// out in a reload must not nag the operator to restart a process that is
// already listening exactly there.
func TestHTTPTopologyRestartReason_ComparesEffectiveAddresses(t *testing.T) {
	boot := &ports.HTTPConfig{}
	running := &ports.HTTPConfig{AdminAddr: defaultAdminAddr, MonitorAddr: defaultMonitorAddr}

	assert.Empty(t, httpTopologyRestartReason(boot, running),
		"spelling out the default addresses is not a topology change")
	assert.Empty(t, httpTopologyRestartReason(running, boot),
		"and neither is omitting them again")
}

// TestHTTPTopologyRestartReason_IgnoresAPIKeyRotation proves key rotation is
// NOT topology: only the fields bound into the listeners at startup count, so a
// rotated admin key does not nag the operator to restart.
func TestHTTPTopologyRestartReason_IgnoresAPIKeyRotation(t *testing.T) {
	boot := &ports.HTTPConfig{AdminAddr: ":8080", AdminAPIKey: shared.NewSecret("boot-key-0123456789")}
	running := &ports.HTTPConfig{AdminAddr: ":8080", AdminAPIKey: shared.NewSecret("rotated-key-0123456789")}

	assert.Empty(t, httpTopologyRestartReason(boot, running))
}

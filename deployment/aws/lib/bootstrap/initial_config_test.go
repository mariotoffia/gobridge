package bootstrap

import (
	"context"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	infra "github.com/mariotoffia/gobridge/deployment/aws/infra"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestRepositoryAbsenceIdlesAndSameConfigReactivates(t *testing.T) {
	a := NewApp(infra.BootstrapConfig{BridgeID: "idle"})
	a.manager = config.NewManager(config.Layer{})
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "idle"}}
	a.parameterResolver = staticParameterResolver{a.cfg.AdminAPIKeyParam: "admin-secret-key-123456"}
	require.NoError(t, a.applyLogicalConfig(t.Context(), cfg, false))
	previous := a.CurrentRuntime()
	t.Cleanup(func() { _ = stopRuntime(context.Background(), a.CurrentRuntime(), cfg) })
	require.True(t, a.activated.Load())
	a.missing.Store(true)
	previous.Fence()
	require.True(t, a.idleRepository(t.Context()))
	require.Nil(t, a.CurrentRuntime())
	require.Nil(t, a.CurrentAppliedConfig())
	require.False(t, previous.IsRunning())
	require.True(t, a.activated.Load(), "idle must not reopen the initializer")
	a.missing.Store(false)
	require.NoError(t, a.activateRepository(t.Context(), cfg))
	require.NotSame(t, previous, a.CurrentRuntime())
}

func TestStalePlanCannotInstallAfterConfigWithdrawal(t *testing.T) {
	a := NewApp(infra.BootstrapConfig{BridgeID: "withdrawn"})
	plan := &runtimePlan{epoch: a.observationEpoch.Load()}
	a.observationEpoch.Add(1)
	require.Error(t, a.installPlan(plan))
	require.Nil(t, a.CurrentRuntime())
	require.False(t, a.activated.Load())
}

func TestCoordinatedAbsenceRequestsProcessExit(t *testing.T) {
	a := NewApp(infra.BootstrapConfig{BridgeID: "cohort"})
	a.rolloutDriver = &bridge.ClusterRolloutDriver{}
	require.False(t, a.idleRepository(t.Context()))
	require.True(t, a.runtimeTerminal())
	select {
	case <-a.terminalCh:
	default:
		t.Fatal("absence did not request process exit")
	}
}

func awaitApplied(t *testing.T, a *App) {
	t.Helper()
	wait.Until(t, time.Second, "first configuration activation", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.appliedRef.Get() != nil
	})
}

func TestIntegration_InitialSourceRespectsTargetAndWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback bootstrap listeners")
	}
	for _, tc := range []struct {
		name      string
		role      infra.NodeRole
		existing  bool
		wantLoads int32
	}{
		{"control initializes", infra.NodeRoleControl, false, 1},
		{"existing target wins", infra.NodeRoleControl, true, 0},
		{"worker waits", infra.NodeRoleWorker, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge.yaml")
			cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "initial"}}
			if tc.existing {
				require.NoError(t, parser.WriteFile(path, cfg))
			}
			var loads atomic.Int32
			source := configLoaderFunc(func(context.Context) (*ports.BridgeConfig, error) { loads.Add(1); return cfg, nil })
			a := NewApp(infra.BootstrapConfig{BridgeID: "initial", NodeRole: tc.role, ConfigFilePath: path,
				AdminAddr: "127.0.0.1:0", MonitorAddr: "127.0.0.1:0", TransportHTTPAddr: "127.0.0.1:0", AdminAPIKeyParam: "/admin", PollInterval: "1s"},
				WithInitialConfigSource(source), WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))
			clk := clocktest.New()
			a.clk = clk
			require.NoError(t, a.Start(t.Context()))
			t.Cleanup(func() { require.NoError(t, a.Stop(context.Background())) })
			if tc.existing {
				awaitApplied(t, a)
			} else {
				wait.Until(t, time.Second, "absence observed", a.missing.Load)
				if tc.role == infra.NodeRoleControl {
					wait.Until(t, time.Second, "initial source activates", func() bool { clk.Advance(time.Second); return a.CurrentRuntime() != nil })
				} else {
					clk.Advance(10 * time.Second)
					require.Nil(t, a.CurrentRuntime())
				}
			}
			require.Equal(t, tc.wantLoads, loads.Load())
			if a.CurrentRuntime() != nil {
				require.NoError(t, os.Remove(path))
				wait.Until(t, time.Second, "deletion idles data plane", func() bool { clk.Advance(time.Second); return a.CurrentRuntime() == nil })
				clk.Advance(10 * time.Second)
				require.Equal(t, tc.wantLoads, loads.Load(), "idle never reopens initialization")
			}
		})
	}
}

func TestShutdownDoesNotWaitForeverForRepositoryRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	source := &blockedConfigObserver{entered: make(chan struct{}), release: make(chan struct{})}
	manager := config.NewManager(config.Layer{Name: "blocked", Loader: source})
	_, err := manager.Observe(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); close(source.release); manager.Stop() })
	wait.RequireClosed(t, source.entered, time.Second)
	app := NewApp(infra.BootstrapConfig{BridgeID: "blocked"})
	app.manager, app.started, app.watchCancel = manager, true, cancel
	stopCtx, stop := context.WithCancel(t.Context())
	stop()
	require.ErrorIs(t, app.Stop(stopCtx), context.Canceled)
}

func TestStartupHealthDistinguishesWaitingFromRefusal(t *testing.T) {
	app := NewApp(infra.BootstrapConfig{BridgeID: "startup-health"})
	for _, tc := range []struct {
		name    string
		err     error
		pending bool
	}{
		{"clean wait", nil, true},
		{"repository unavailable", shared.ErrUnavailable, true},
		{"rejected blueprint", &ports.BlueprintValidationError{Errors: []string{"invalid route"}}, false},
		{"denied credentials", shared.ErrNotAuthorized, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app.repositoryError(tc.err)
			status := app.configWatchHealth()
			require.Equal(t, tc.pending, status.StartupPending)
			require.Equal(t, tc.err != nil, status.Degraded)
		})
	}
	app.activated.Store(true)
	app.repositoryError(shared.ErrUnavailable)
	require.False(t, app.configWatchHealth().StartupPending, "a live reconfiguration fault is not startup waiting")
	app.activated.Store(false)
	app.wedged.Store(true)
	app.repositoryError(nil)
	require.False(t, app.configWatchHealth().StartupPending, "terminal failure must never be hidden as waiting")
}

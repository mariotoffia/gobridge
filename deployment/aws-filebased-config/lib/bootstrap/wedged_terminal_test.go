package bootstrap

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestRecoverPrevious_GiveUpMarksWedgedAndTerminal is the core
// regression: when a prepare/commit swap has stopped the old runtime and its
// recoverPrevious cannot restore one (here the nil-previous give-up path), the
// App must latch WEDGED so runtimeTerminal reports terminal. Before the fix the
// give-up paths left runtimeRef nil with no flag, and runtimeTerminal treated
// nil as a transient swap window — so the backstop never fired and the task
// bridged nothing forever.
func TestRecoverPrevious_GiveUpMarksWedgedAndTerminal(t *testing.T) {
	app := NewApp(testBootstrapCfg())

	// A fresh App with no runtime is in a transient (pre-first-swap) state,
	// NOT terminal — the backstop must not fire on a plain nil runtime.
	require.False(t, app.runtimeTerminal(), "nil runtime without wedge is transient, not terminal")

	// recoverPrevious with no previous config to recover to is a give-up path.
	app.recoverPrevious(context.Background(), nil)

	require.True(t, app.wedged.Load(), "give-up path must latch the wedged flag")
	require.True(t, app.runtimeTerminal(), "wedged App must report terminal so the backstop fires")
	require.Nil(t, app.CurrentRuntime(), "wedged App has no active runtime")
}

// TestInstallPlan_ClearsWedged proves a later successful apply un-latches the
// wedged flag so a recovered App stops reporting terminal.
func TestInstallPlan_ClearsWedged(t *testing.T) {
	app := NewApp(testBootstrapCfg())
	app.wedged.Store(true)
	require.True(t, app.runtimeTerminal())

	// installPlan is the common success path; a minimal plan with an
	// HTTP-less registry (transportHandler falls back to NotFoundHandler) is
	// enough to exercise the flag clear without building a real runtime.
	app.installPlan(&runtimePlan{
		logical:  &ports.BridgeConfig{},
		inputs:   &resolvedInputs{},
		registry: &factoryRegistry{cfg: &ports.BridgeConfig{}},
	})

	require.False(t, app.wedged.Load(), "successful apply must clear the wedged latch")
	require.False(t, app.runtimeTerminal())
}

// TestWatchTerminal_FiresWhenWedged drives the injected clock: once the App is
// wedged, the terminal backstop must sample it and signal terminalCh so Run
// exits non-zero. Uses clocktest.Fake with TickerCount to synchronise with the
// backstop goroutine's ticker registration before advancing time.
func TestWatchTerminal_FiresWhenWedged(t *testing.T) {
	fake := clocktest.New()
	app := NewApp(testBootstrapCfg(), WithTerminalPollInterval(time.Second))
	app.clk = fake
	app.wedged.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.watchTerminal(ctx)

	// Spin until the backstop has registered its ticker on the fake clock, so
	// the Advance below is guaranteed to fire a tick (no lost-tick race).
	require.Eventually(t, func() bool { return fake.TickerCount() >= 1 },
		time.Second, time.Millisecond)
	fake.Advance(time.Second)

	wait.RequireReceive(t, app.terminalCh, time.Second)
}

// TestTerminalRuntime_Sentinel checks the sentinel exposed through
// RuntimeProvider reports the fail-closed projection every probe relies on.
func TestTerminalRuntime_Sentinel(t *testing.T) {
	var rt ports.Runtime = terminalRuntime{}

	require.True(t, rt.Terminal(), "sentinel must report terminal so /live fails closed")
	require.False(t, rt.Healthy())
	require.False(t, rt.IsRunning())
	require.Equal(t, ports.LevelDown, rt.ReadinessLevel(context.Background()))
	require.False(t, rt.DeepHealth(context.Background()).ReadyForTraffic)
	require.Nil(t, rt.Routes())
	require.Contains(t, rt.ComponentErrors(), "bootstrap")
}

// TestApp_LiveProbeFailsClosedWhenWedged is the end-to-end regression on
// the /live side: a running App answers /live 200; a transient nil runtime
// (swap window) still answers 200; but a WEDGED App answers 503 so an
// orchestrator with a liveness probe restarts the dead-but-serving task. This
// composes with httpapi unchanged — the sentinel drives the existing
// "rt != nil && rt.Terminal()" branch in handleLive.
func TestApp_LiveProbeFailsClosedWhenWedged(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-wedged",
		ConfigFilePath:    cfgPath,
		PollInterval:      "1h", // keep the watcher dormant
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}, WithParameterResolver(staticParameterResolver{
		"/admin": "admin-secret-key-123456",
	}))

	require.NoError(t, app.Start(t.Context()))
	realRT := app.CurrentRuntime()
	require.NotNil(t, realRT)
	t.Cleanup(func() {
		// Restore the real runtime so Stop tears it down (the test detaches it
		// from runtimeRef below to simulate the swap/wedge windows).
		app.runtimeRef.Set(realRT)
		app.wedged.Store(false)
		_ = app.Stop(context.Background())
	})

	liveURL := app.MonitorURL() + "/api/v1/monitor/live"

	require.Equal(t, http.StatusOK, getStatus(t, liveURL), "running App must be live")

	// Transient nil during a swap window: still live (recoverable).
	app.runtimeRef.Set(nil)
	require.Equal(t, http.StatusOK, getStatus(t, liveURL),
		"a transient nil runtime (swap window) must stay live")

	// Wedged: swap + recovery both failed. /live must fail closed.
	app.wedged.Store(true)
	require.Equal(t, http.StatusServiceUnavailable, getStatus(t, liveURL),
		"a wedged App must report not-live so the orchestrator restarts the task")
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test probe against a local ephemeral server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

package bootstrap

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// writeBridgeConfig seeds a minimal, valid bridge config file and returns its
// path.
func writeBridgeConfig(t *testing.T, logLevel string) string {
	t.Helper()
	path := t.TempDir() + "/bridge.yaml"
	require.NoError(t, parser.WriteFile(path, &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-lifecycle",
			DeploymentMode: "standalone",
			LogLevel:       logLevel,
		},
	}))
	return path
}

// TestAppStart_ListenerCollisionStopsTheInstalledRuntime: Start installs the
// runtime BEFORE it opens the transport, admin and monitor listeners and before
// it starts the config watcher. Every failure after that point returned an error
// while leaving the runtime running — sessions connected, stores open, drainers
// and lease renewals live — with no reference left to stop it. A supervisor that
// retries Start (or a caller that falls back to another port) then runs two
// runtimes against the same brokers and leases.
func TestAppStart_ListenerCollisionStopsTheInstalledRuntime(t *testing.T) {
	// Each listener is opened at a different point after the runtime is
	// installed, so each is its own leak path.
	for _, tc := range []struct {
		name     string
		listener string
	}{
		{name: "transport", listener: "transport"},
		{name: "admin", listener: "admin"},
		{name: "monitor", listener: "monitor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = occupied.Close() })
			taken := occupied.Addr().String()

			bootCfg := deployinfra.BootstrapConfig{
				BridgeID:          "bridge-lifecycle",
				ConfigFilePath:    writeBridgeConfig(t, "info"),
				PollInterval:      "1h",
				AdminAddr:         ":0",
				MonitorAddr:       ":0",
				TransportHTTPAddr: ":0",
				AdminAPIKeyParam:  "/admin",
			}
			switch tc.listener {
			case "transport":
				bootCfg.TransportHTTPAddr = taken
			case "admin":
				bootCfg.AdminAddr = taken
			case "monitor":
				bootCfg.MonitorAddr = taken
			}

			app := NewApp(bootCfg,
				WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))

			err = app.Start(t.Context())
			require.Error(t, err, "test setup: the colliding %s listener must fail Start", tc.listener)

			assert.Nil(t, app.CurrentRuntime(),
				"a failed Start must leave no installed runtime behind")
		})
	}
}

// TestApplyLogicalConfig_RejectedReloadLeavesLogLevelUnchanged: bridge.log_level
// was applied at the TOP of the reload path, before deployment-profile
// validation and before anything was built. A rejected candidate therefore
// changed live process verbosity while the desired and running state stayed on
// the old config — the running system no longer matched any config an operator
// could read back.
func TestApplyLogicalConfig_RejectedReloadLeavesLogLevelUnchanged(t *testing.T) {
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)

	bootCfg := testBootstrapCfg()
	bootCfg.Topology = deployinfra.TopologyFilesystemReplicated
	app := NewApp(bootCfg,
		WithLogLevelVar(lv),
		WithLogger(slog.New(slog.NewJSONHandler(discard{}, &slog.HandlerOptions{Level: lv}))),
	)

	// A candidate the filesystem-replicated profile refuses (it provisions no
	// distributed outbox store): it raises the log level AND fails validation.
	rejected := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-x",
			DeploymentMode: "standalone",
			LogLevel:       "debug",
		},
		Routes: []ports.RouteDef{{ID: "r1", DeliveryMode: "shared_outbox"}},
	}

	err := app.applyLogicalConfig(context.Background(), rejected, false)
	require.Error(t, err, "test setup: the candidate must be rejected by the deployment profile")

	assert.Equal(t, slog.LevelWarn, lv.Level(),
		"a rejected reload must not mutate live process behaviour; log level is committed with the runtime, not before it")
}

// hangingWatchWait stands in for a watchLoop stuck inside a reload (its own
// teardown paths are deliberately NOT bounded by the process shutdown budget).
// App.Stop waited on that goroutine with no bound at all, so SIGTERM never
// reached the runtime drain, the HTTP shutdown, or the metrics flush.
func TestAppStop_BoundedByShutdownBudgetWhenAReloadIsStuck(t *testing.T) {
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-lifecycle",
		ConfigFilePath:    writeBridgeConfig(t, "info"),
		PollInterval:      "1h",
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}, WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))

	require.NoError(t, app.Start(t.Context()))

	// A watcher goroutine that will not observe cancellation, exactly as a
	// reload blocked in an unbounded plugin teardown behaves.
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	app.watchWg.Go(func() { <-stuck })

	stopCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- app.Stop(stopCtx) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("App.Stop waited on a stuck reload without bound; the process shutdown budget must cover it")
	}
}

// TestAppStop_SurfacesTheRuntimeDrainError is the SIGTERM truthfulness contract
// for the shipped `gobridge-filebased` binary. Stop cancels the watcher context
// first, which the runtime turns into a Stop of its own, so App's own
// stopRuntime is often the SECOND caller. A second Stop that returned nil made
// Run report a clean exit over a drain that had in fact failed — an operator
// reading the exit code and the final log line could not tell a settled
// shutdown from an aborted one.
func TestAppStop_SurfacesTheRuntimeDrainError(t *testing.T) {
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-lifecycle",
		ConfigFilePath:    writeBridgeConfig(t, "info"),
		PollInterval:      "1h",
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}, WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))

	require.NoError(t, app.Start(t.Context()))

	// Swap in a runtime whose disconnect fails, standing in for a hung broker
	// close on the way out.
	failing := goruntime.New(goruntime.WithInstanceID("drain-fails"))
	require.NoError(t, failing.AddRoute(
		goruntime.RouteConfig{ID: "r1"},
		nil, nil, &closeFailSession{events: make(chan ports.SessionEvent, 1)}, nil,
	))
	app.runtimeRef.Set(failing)

	err := app.Stop(context.Background())
	require.Error(t, err, "App.Stop must report a runtime drain that failed, not a clean shutdown")
}

// TestAppCommit_RuntimeOutlivesTheApplyContext: an admin config commit applies
// in-band on a context the httpapi transaction detaches from the request but
// still bounds with its apply deadline and cancels the instant Commit returns.
// Starting the new runtime on it tied the runtime's lifetime to the apply — the
// start-context watcher saw the cancel and stopped the freshly installed
// runtime, so the process was left installed-but-stopped with /live still 200
// (a clean stop is not terminal) and /ready false, bridging nothing until an
// unrelated config arrived. A committed runtime must outlive the apply that
// built it.
func TestAppCommit_RuntimeOutlivesTheApplyContext(t *testing.T) {
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-lifecycle",
		ConfigFilePath:    writeBridgeConfig(t, "info"),
		PollInterval:      "1h",
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}, WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	const key = "admin-secret-key-123456"
	txnBase := app.AdminURL() + "/api/v1/admin/config/transactions"

	resp, body := adminJSON(t, http.MethodPost, txnBase, key, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	txnID, _ := body["txn_id"].(string)
	require.NotEmpty(t, txnID)

	resp, _ = adminJSON(t, http.MethodPatch, txnBase+"/"+txnID, key, `{"bridge":{"log_level":"debug"}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body = adminJSON(t, http.MethodPost, txnBase+"/"+txnID+"/commit", key, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "committed", body["status"])

	rt := app.CurrentRuntime()
	require.NotNil(t, rt, "the commit must install a runtime")

	// The commit handler has returned, so its apply context is cancelled. The
	// runtime must stay running.
	assert.True(t, wait.StableFor(t, rt.IsRunning, 300*time.Millisecond, 5*time.Second),
		"the committed runtime stopped itself when the apply context was cancelled")
}

// TestAppStart_AdoptsTheConfiguredProcessShutdownBudget: bridge.shutdown_timeout
// is documented as the total grace period for a clean shutdown, and App.Run
// spends the App's budget on the whole SIGTERM path — config watcher, rollout
// drive, HTTP servers, runtime drain, stores, telemetry. While that budget was
// an invisible 30s constant, the documented field governed nothing. An explicit
// WithShutdownTimeout still wins, so library and test callers are unaffected.
func TestAppStart_AdoptsTheConfiguredProcessShutdownBudget(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	require.NoError(t, parser.WriteFile(cfgPath, &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              "bridge-lifecycle",
			DeploymentMode:  "standalone",
			ShutdownTimeout: "12s",
		},
	}))
	bootCfg := deployinfra.BootstrapConfig{
		BridgeID:          "bridge-lifecycle",
		ConfigFilePath:    cfgPath,
		PollInterval:      "1h",
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}
	resolver := WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"})

	adopted := NewApp(bootCfg, resolver)
	require.NoError(t, adopted.Start(t.Context()))
	t.Cleanup(func() { _ = adopted.Stop(context.Background()) })
	assert.Equal(t, 12*time.Second, adopted.shutdownTimeout,
		"the App must spend the budget bridge.shutdown_timeout declares, not an invisible default")

	pinned := NewApp(bootCfg, resolver, WithShutdownTimeout(3*time.Second))
	require.NoError(t, pinned.Start(t.Context()))
	t.Cleanup(func() { _ = pinned.Stop(context.Background()) })
	assert.Equal(t, 3*time.Second, pinned.shutdownTimeout,
		"an explicit WithShutdownTimeout must still win over the config field")
}

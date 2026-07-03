package bootstrap

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// TestApp_CommitAppliesExactlyOnceWithActiveWatcher is the NEW-DEFECT
// regression for the double-apply introduced by ConfigApplier wiring (FIX 2):
//
// applyCommittedConfig writes the committed config to disk (changing its
// content hash) and applies it in-band. With an ACTIVE poll watcher, the
// watcher then detects the hash change and re-emits the same config; before the
// fix, watchLoop applied it UNCONDITIONALLY, so every admin commit triggered a
// SECOND full stop→rebuild→start swap (a redundant outage plus doubled exposure
// to the swap-failure→wedge path). The original FIX 2 test masked this by
// pinning PollInterval to "1h" (dormant watcher).
//
// This test runs a live watcher (short poll) and asserts:
//   - an admin commit rebuilds the runtime EXACTLY ONCE (the in-band apply);
//     the watcher's re-emit is recognised as already-applied and skipped;
//   - a genuine external disk edit STILL triggers a rebuild (the idempotency
//     skip must not eat real changes).
func TestApp_CommitAppliesExactlyOnceWithActiveWatcher(t *testing.T) {
	cfgPath := t.TempDir() + "/bridge.yaml"
	seed := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-once",
			DeploymentMode: "standalone",
			LogLevel:       "info",
		},
	}
	require.NoError(t, parser.WriteFile(cfgPath, seed))

	var rebuilds, skips atomic.Int64
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          "bridge-once",
		ConfigFilePath:    cfgPath,
		PollInterval:      "20ms", // ACTIVE watcher: re-emits the committed file
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}, WithParameterResolver(staticParameterResolver{
		"/admin": "admin-secret-key-123456",
	}))
	app.onRuntimeInstalled = func() { rebuilds.Add(1) }
	app.onReloadSkipped = func() { skips.Add(1) }

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() {
		_ = app.Stop(context.Background())
	})

	// Baseline: the initial runtime install happened during Start.
	require.Eventually(t, func() bool { return rebuilds.Load() >= 1 }, 3*time.Second, 10*time.Millisecond)
	// Reset counters so the assertions below measure only post-commit activity.
	rebuilds.Store(0)
	skips.Store(0)

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

	// The in-band applier converged the runtime.
	require.Equal(t, "debug", app.CurrentAppliedConfig().Bridge.LogLevel)

	// Wait until the active watcher has re-emitted the committed file and had
	// it recognised as already-applied (skipped). This deterministically proves
	// the watcher observed the post-commit file without triggering a rebuild.
	require.Eventually(t, func() bool { return skips.Load() >= 1 }, 3*time.Second, 10*time.Millisecond)

	// The commit must have caused EXACTLY ONE runtime rebuild (the in-band
	// apply); the watcher's re-emit must not have rebuilt again.
	require.Equal(t, int64(1), rebuilds.Load(),
		"an admin commit must trigger exactly one runtime rebuild; the poll watcher's re-emit of the committed file must be skipped, not rebuilt")

	// A genuine external disk edit must STILL trigger a rebuild — the skip must
	// not eat real changes.
	external := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-once",
			DeploymentMode: "standalone",
			LogLevel:       "warn",
		},
	}
	require.NoError(t, parser.WriteFile(cfgPath, external))

	require.Eventually(t, func() bool {
		applied := app.CurrentAppliedConfig()
		return applied != nil && applied.Bridge.LogLevel == "warn"
	}, 3*time.Second, 10*time.Millisecond)
	require.GreaterOrEqual(t, rebuilds.Load(), int64(2),
		"a genuine external disk edit must trigger a runtime rebuild (the idempotency skip must not eat real changes)")
}

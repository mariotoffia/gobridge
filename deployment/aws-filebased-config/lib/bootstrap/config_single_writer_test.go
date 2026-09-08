package bootstrap

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// TestApp_ConfigSingleWriter_ControlCommitsDurably proves the constructed
// httpapi.Config carries ConfigSingleWriter=true for the control/single node:
// a durable config-transaction commit against the non-CAS parser.FileStore is
// PERMITTED (200 committed) only because the App asserted single-writer.
//
// Mutation guard: revert the ConfigSingleWriter wiring (or force it false) and
// the commit is refused with 500 — this assertion fails.
func TestApp_ConfigSingleWriter_ControlCommitsDurably(t *testing.T) {
	app := startSingleWriterApp(t, deployinfra.NodeRoleControl, "bridge-writer")

	const key = "admin-secret-key-123456"
	txnBase := app.AdminURL() + "/api/v1/admin/config/transactions"

	resp, body := adminJSON(t, http.MethodPost, txnBase, key, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	txnID, _ := body["txn_id"].(string)
	require.NotEmpty(t, txnID)

	resp, _ = adminJSON(t, http.MethodPatch, txnBase+"/"+txnID, key, `{"bridge":{"log_level":"debug"}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body = adminJSON(t, http.MethodPost, txnBase+"/"+txnID+"/commit", key, "")
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"control node must be single-writer so the non-CAS FileStore commit is permitted")
	require.Equal(t, "committed", body["status"])
}

// TestApp_ConfigSingleWriter_WorkerCommitFailsClosed proves the constructed
// httpapi.Config carries ConfigSingleWriter=false for a worker node: a durable
// commit against the non-CAS FileStore is REFUSED (fail closed), because a
// worker is not the sole durable writer (its EFS mount is read-only in a real
// cluster). This is the correct posture — not a silent last-writer-wins Save.
func TestApp_ConfigSingleWriter_WorkerCommitFailsClosed(t *testing.T) {
	app := startSingleWriterApp(t, deployinfra.NodeRoleWorker, "bridge-worker")

	const key = "admin-secret-key-123456"
	txnBase := app.AdminURL() + "/api/v1/admin/config/transactions"

	resp, body := adminJSON(t, http.MethodPost, txnBase, key, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	txnID, _ := body["txn_id"].(string)
	require.NotEmpty(t, txnID)

	resp, _ = adminJSON(t, http.MethodPatch, txnBase+"/"+txnID, key, `{"bridge":{"log_level":"debug"}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, _ = adminJSON(t, http.MethodPost, txnBase+"/"+txnID+"/commit", key, "")
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"a non-writer worker must fail closed on a non-CAS store commit, not perform a last-writer-wins Save")
}

// startSingleWriterApp boots an App for the given NodeRole with a seeded config
// file and a dormant watcher, returning a started App with Stop registered.
func startSingleWriterApp(t *testing.T, role deployinfra.NodeRole, bridgeID string) *App {
	t.Helper()
	cfgPath := t.TempDir() + "/bridge.yaml"
	seed := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             bridgeID,
			DeploymentMode: "standalone",
			LogLevel:       "info",
		},
	}
	require.NoError(t, parser.WriteFile(cfgPath, seed))

	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          bridgeID,
		NodeRole:          role,
		ConfigFilePath:    cfgPath,
		PollInterval:      "1h", // watcher dormant: isolate the commit path
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}, WithParameterResolver(staticParameterResolver{
		"/admin": "admin-secret-key-123456",
	}))

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	return app
}

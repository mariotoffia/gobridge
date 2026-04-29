package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleConfigTxnCommit_VersionConflict(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, path := newConfigTestServer(t, cfg)

	// Start a transaction (captures base version 0).
	txnID := createTxn(t, s)
	applyPatch(t, s, txnID, `{"bridge": {"id": "test-bridge", "log_level": "warn"}}`)

	// Simulate another instance committing: write version 1 to disk.
	diskCfg := sampleBridgeConfig()
	diskCfg.Version = 1
	diskCfg.Bridge.LogLevel = "info"
	require.NoError(t, config.WriteFile(path, diskCfg))

	// Our commit should fail with 409.
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txnID+"/commit")
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnCommit(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Contains(t, body["error"], "version conflict")

	// Verify file was NOT overwritten.
	parsed, err := config.ParseFile(path, config.FormatYAML)
	require.NoError(t, err)
	assert.Equal(t, 1, parsed.Version)
	assert.Equal(t, "info", parsed.Bridge.LogLevel)
}

func TestHandleConfigTxnCommit_SequentialVersionIncrement(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, path := newConfigTestServer(t, cfg)

	// First commit: version 0 → 1.
	txnID := createTxn(t, s)
	applyPatch(t, s, txnID, `{"bridge": {"id": "test-bridge", "log_level": "debug"}}`)
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txnID+"/commit")
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnCommit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Update the provider to return the committed config (as the
	// real system would after the file watcher picks up the change).
	parsed, err := config.ParseFile(path, config.FormatYAML)
	require.NoError(t, err)
	assert.Equal(t, 1, parsed.Version)
	currentCfg := parsed

	// Recreate the server's config provider to return the new version.
	s.configTxn = newTxnManager(&config.FileStore{Path: path}, func() *ports.BridgeConfig { return currentCfg }, nil)

	// Second commit: version 1 → 2.
	txnID2 := createTxn(t, s)
	applyPatch(t, s, txnID2, `{"bridge": {"id": "test-bridge", "log_level": "error"}}`)
	rec2 := httptest.NewRecorder()
	req2 := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txnID2+"/commit")
	req2.SetPathValue("txnID", txnID2)
	s.handleConfigTxnCommit(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&body))
	assert.Equal(t, float64(2), body["version"])

	parsed2, err := config.ParseFile(path, config.FormatYAML)
	require.NoError(t, err)
	assert.Equal(t, 2, parsed2.Version)
	assert.Equal(t, "error", parsed2.Bridge.LogLevel)
}

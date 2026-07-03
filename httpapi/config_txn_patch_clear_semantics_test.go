package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestConfigTxn_Patch_CannotClearFields_HTTP is the HTTP-level regression for
// item (6): PATCH is merge-only, so an empty-string scalar and an echoed-back
// redaction marker both PRESERVE the current value rather than clearing it. A
// client cannot wipe a field via PATCH; only a full replacement can.
func TestConfigTxn_Patch_CannotClearFields_HTTP(t *testing.T) {
	cfg := sampleBridgeConfig()
	cfg.HTTP.CORSOrigins = "https://ops.example.com"
	s, _ := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)

	// Empty-string overlay for a non-secret scalar: keeps the base value.
	applyPatch(t, s, txnID, `{"http":{"cors_origins":""}}`)
	// Redaction-marker overlay for a secret: keeps the stored secret. NOTE the
	// marker is "[REDACTED]" (the value a redacted read echoes back), NOT "***".
	applyPatch(t, s, txnID, `{"http":{"admin_api_key":"[REDACTED]"}}`)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodGet, "/api/v1/admin/config/transactions/"+txnID)
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnGet(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	preview, ok := body["preview"].(map[string]any)
	require.True(t, ok, "preview must be present")
	httpBlock, ok := preview["http"].(map[string]any)
	require.True(t, ok, "preview.http must be present")

	assert.Equal(t, "https://ops.example.com", httpBlock["cors_origins"],
		"empty-string PATCH must NOT clear cors_origins")
	// A preserved (populated) secret redacts to the marker; a cleared secret
	// would be absent/empty. Presence of the marker proves it was not cleared.
	assert.Equal(t, "[REDACTED]", httpBlock["admin_api_key"],
		`"[REDACTED]" PATCH must keep the stored admin_api_key`)
}

// TestConfigTxn_Patch_ClearSemantics_ByteLevel proves the documented semantics
// at the merge level where the secret can actually be revealed: the HTTP preview
// always redacts, so it cannot distinguish "kept the base key" from "overwrote
// with the literal marker". Reveal() pins that the redaction-marker/empty overlay
// preserves the exact base bytes, while any other non-empty value overwrites.
func TestConfigTxn_Patch_ClearSemantics_ByteLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := sampleBridgeConfig()
	cfg.Version = 1
	cfg.HTTP.CORSOrigins = "https://ops.example.com"
	require.NoError(t, parser.WriteFile(path, cfg))

	store := &parser.FileStore{Path: path, Registry: newTestRegistry(t)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return cfg }, nil, nil, clk)
	ctx := context.Background()

	t.Run("empty and redaction-marker overlays preserve", func(t *testing.T) {
		txn, err := mgr.Begin(ctx, 0)
		require.NoError(t, err)
		defer func() { _ = mgr.Rollback(txn.ID) }()

		overlay := &ports.BridgeConfig{HTTP: &ports.HTTPConfig{
			CORSOrigins: "",                      // empty → keep base
			AdminAPIKey: shared.RedactedSecret(), // marker → keep base secret
		}}
		_, _, err = mgr.Patch(ctx, txn.ID, overlay)
		require.NoError(t, err)

		preview, err := mgr.Preview(ctx, txn.ID)
		require.NoError(t, err)
		assert.Equal(t, "https://ops.example.com", preview.HTTP.CORSOrigins)
		assert.Equal(t, "super-secret-admin-key-1234", preview.HTTP.AdminAPIKey.Reveal(),
			"redaction-marker overlay must preserve the exact stored secret")
	})

	t.Run("a real non-marker value overwrites", func(t *testing.T) {
		txn, err := mgr.Begin(ctx, 0)
		require.NoError(t, err)
		defer func() { _ = mgr.Rollback(txn.ID) }()

		overlay := &ports.BridgeConfig{HTTP: &ports.HTTPConfig{
			AdminAPIKey: shared.NewSecret("rotated-key-9999"),
		}}
		_, _, err = mgr.Patch(ctx, txn.ID, overlay)
		require.NoError(t, err)

		preview, err := mgr.Preview(ctx, txn.ID)
		require.NoError(t, err)
		assert.Equal(t, "rotated-key-9999", preview.HTTP.AdminAPIKey.Reveal(),
			"a real (non-marker) secret value must overwrite — PATCH is not read-only for secrets")
	})
}

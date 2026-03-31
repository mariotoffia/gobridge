package integration_test

import (
	"net/http"
	"testing"
	"time"
)

// ===============================================================
// Group 1: Config API CRUD Lifecycle
//
// Validates the full HTTP round-trip for each config management
// endpoint over a real TCP connection with real auth middleware.
// ===============================================================

// TestConfigAPI_GetConfig_ReturnsCurrentConfig validates that GET /config
// returns the effective config via a real HTTP connection.
func TestConfigAPI_GetConfig_ReturnsCurrentConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	resp, body := apiGet(t, srv.URL+"/api/v1/admin/config", testAdminAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /config: got %d, want 200", resp.StatusCode)
	}

	cfg, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatal("response missing 'config' object")
	}
	bridge := cfg["bridge"].(map[string]any)
	if bridge["id"] != "api-test-bridge" {
		t.Errorf("bridge.id: got %v, want api-test-bridge", bridge["id"])
	}
}

// TestConfigAPI_CreateTransaction_Returns201WithTxnID validates that
// POST /transactions returns 201 with a transaction ID.
func TestConfigAPI_CreateTransaction_Returns201WithTxnID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	resp, body := apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /transactions: got %d, want 201", resp.StatusCode)
	}

	txnID, ok := body["txn_id"].(string)
	if !ok || txnID == "" {
		t.Fatal("response missing non-empty 'txn_id'")
	}
	if body["created_at"] == nil {
		t.Error("response missing 'created_at'")
	}
	if body["expires_at"] == nil {
		t.Error("response missing 'expires_at'")
	}
	if body["config"] == nil {
		t.Error("response missing 'config' snapshot")
	}
}

// TestConfigAPI_CreateTransaction_WithCustomTTL validates that a custom
// TTL is respected in the transaction expiry.
func TestConfigAPI_CreateTransaction_WithCustomTTL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	resp, body := apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, map[string]string{"ttl": "2m"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /transactions: got %d, want 201", resp.StatusCode)
	}

	createdAt, _ := time.Parse(time.RFC3339, body["created_at"].(string))
	expiresAt, _ := time.Parse(time.RFC3339, body["expires_at"].(string))
	diff := expiresAt.Sub(createdAt)
	if diff < 119*time.Second || diff > 121*time.Second {
		t.Errorf("TTL: got %v, want ~2m", diff)
	}
}

// TestConfigAPI_GetTransaction_ReturnsPatchCountAndPreview validates that
// GET /transactions/{txnID} returns state including patch_count.
func TestConfigAPI_GetTransaction_ReturnsPatchCountAndPreview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	// Apply one patch.
	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "debug"},
	})

	resp, body := apiGet(t, srv.URL+"/api/v1/admin/config/transactions/"+txnID, testAdminAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /transactions/{txnID}: got %d, want 200", resp.StatusCode)
	}

	if body["txn_id"] != txnID {
		t.Errorf("txn_id: got %v, want %s", body["txn_id"], txnID)
	}
	if body["patch_count"] != float64(1) {
		t.Errorf("patch_count: got %v, want 1", body["patch_count"])
	}
	if body["preview"] == nil {
		t.Error("response missing 'preview'")
	}
}

// TestConfigAPI_PatchTransaction_ReturnsMergedPreview validates that
// PATCH returns a merged preview with the overlay applied.
func TestConfigAPI_PatchTransaction_ReturnsMergedPreview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	overlay := map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "debug"},
	}
	resp, body := apiPatch(t, srv.URL+"/api/v1/admin/config/transactions/"+txnID, testAdminAPIKey, overlay)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH: got %d, want 200", resp.StatusCode)
	}

	preview := body["preview"].(map[string]any)
	bridge := preview["bridge"].(map[string]any)
	if bridge["log_level"] != "debug" {
		t.Errorf("preview log_level: got %v, want debug", bridge["log_level"])
	}
	if bridge["id"] != "api-test-bridge" {
		t.Errorf("preview bridge.id: got %v, want api-test-bridge", bridge["id"])
	}
}

// TestConfigAPI_CommitTransaction_Returns200 validates that commit
// succeeds after a valid patch.
func TestConfigAPI_CommitTransaction_Returns200(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "warn"},
	})

	url := srv.URL + "/api/v1/admin/config/transactions/" + txnID + "/commit"
	resp, body := apiPost(t, url, testAdminAPIKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /commit: got %d, want 200, body: %v", resp.StatusCode, body)
	}
	if body["status"] != "committed" {
		t.Errorf("status: got %v, want committed", body["status"])
	}
}

// TestConfigAPI_RollbackTransaction_Returns200 validates that rollback
// succeeds and returns the expected status.
func TestConfigAPI_RollbackTransaction_Returns200(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	url := srv.URL + "/api/v1/admin/config/transactions/" + txnID
	resp, body := apiDelete(t, url, testAdminAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: got %d, want 200", resp.StatusCode)
	}
	if body["status"] != "rolled_back" {
		t.Errorf("status: got %v, want rolled_back", body["status"])
	}
}

// TestConfigAPI_RollbackThenNewTransaction_Succeeds validates that
// after rollback, a new transaction can be created.
func TestConfigAPI_RollbackThenNewTransaction_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)
	rollbackTransaction(t, srv.URL, testAdminAPIKey, txnID)

	// Second transaction should succeed.
	resp, _ := apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second POST /transactions: got %d, want 201", resp.StatusCode)
	}
}

// TestConfigAPI_CommitThenNewTransaction_Succeeds validates that
// after commit, a new transaction can be created.
func TestConfigAPI_CommitThenNewTransaction_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)
	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "error"},
	})
	commitTransaction(t, srv.URL, testAdminAPIKey, txnID)

	resp, _ := apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second POST /transactions: got %d, want 201", resp.StatusCode)
	}
}

package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ===============================================================
// Group 6: Config API Edge Cases and Invariants
//
// Validates transaction isolation, TTL expiry, multi-patch
// accumulation, sensitive field redaction, and audit events.
// ===============================================================

// TestConfigAPI_TransactionIsolation_OnlyOneActive validates that
// attempting to create a second transaction returns 409 Conflict.
func TestConfigAPI_TransactionIsolation_OnlyOneActive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	_ = createTransaction(t, srv.URL, testAdminAPIKey)

	resp, body := apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second txn: got %d, want 409, body: %v", resp.StatusCode, body)
	}
}

// TestConfigAPI_TransactionIsolation_WrongTxnID_Returns404 validates
// that operations with a wrong txn ID return 404.
func TestConfigAPI_TransactionIsolation_WrongTxnID_Returns404(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	_ = createTransaction(t, srv.URL, testAdminAPIKey)

	wrongURL := srv.URL + "/api/v1/admin/config/transactions/wrong-id"
	resp, _ := apiPatch(t, wrongURL, testAdminAPIKey, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge"},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong txn ID: got %d, want 404", resp.StatusCode)
	}
}

// TestConfigAPI_TransactionIsolation_ExpiredTxn_Returns404 validates
// that an expired transaction returns 404 on subsequent operations.
func TestConfigAPI_TransactionIsolation_ExpiredTxn_Returns404(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	resp, body := apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, map[string]string{"ttl": "100ms"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d", resp.StatusCode)
	}
	txnID := body["txn_id"].(string)

	time.Sleep(300 * time.Millisecond) // ESSENTIAL: wait for 100ms transaction TTL to expire

	getURL := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", srv.URL, txnID)
	resp2, _ := apiGet(t, getURL, testAdminAPIKey)
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expired txn GET: got %d, want 404", resp2.StatusCode)
	}
}

// TestConfigAPI_TransactionIsolation_AfterExpiry_NewTxnAllowed
// validates that a new transaction can be created after expiry.
func TestConfigAPI_TransactionIsolation_AfterExpiry_NewTxnAllowed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, map[string]string{"ttl": "100ms"})
	time.Sleep(300 * time.Millisecond) // ESSENTIAL: wait for 100ms transaction TTL to expire

	resp, _ := apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("new txn after expiry: got %d, want 201", resp.StatusCode)
	}
}

// TestConfigAPI_MultiPatch_Accumulation validates that three sequential
// patches accumulate correctly in the preview.
func TestConfigAPI_MultiPatch_Accumulation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	// Patch 1: add receiver.
	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"receivers": []map[string]any{{"id": "rx-acc", "transport": "fake"}},
	})
	// Patch 2: add sender.
	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"senders": []map[string]any{{"id": "tx-acc", "transport": "fake"}},
	})
	// Patch 3: add binding + route.
	body := applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bindings": []map[string]any{{"id": "bind-acc", "sender_id": "tx-acc", "address": "addr/acc"}},
		"routes": []map[string]any{
			{"id": "r-acc", "receiver_id": "rx-acc", "delivery_mode": "direct_hold", "bindings": []string{"bind-acc"}},
		},
	})

	preview := body["preview"].(map[string]any)
	receivers := preview["receivers"].([]any)
	senders := preview["senders"].([]any)
	routes := preview["routes"].([]any)

	if len(receivers) < 2 {
		t.Errorf("receivers: got %d, want >= 2 (base + new)", len(receivers))
	}
	if len(senders) < 2 {
		t.Errorf("senders: got %d, want >= 2", len(senders))
	}
	if len(routes) < 2 {
		t.Errorf("routes: got %d, want >= 2", len(routes))
	}
}

// TestConfigAPI_MultiPatch_LastPatchWins validates that when two
// patches modify the same field, the last one wins.
func TestConfigAPI_MultiPatch_LastPatchWins(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "debug"},
	})
	body := applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "warn"},
	})

	preview := body["preview"].(map[string]any)
	bridge := preview["bridge"].(map[string]any)
	if bridge["log_level"] != "warn" {
		t.Errorf("log_level: got %v, want warn (last patch wins)", bridge["log_level"])
	}
}

// TestConfigAPI_Redaction_GetConfig_HidesAPIKeys validates that API
// keys in the HTTP config block are redacted in GET /config.
func TestConfigAPI_Redaction_GetConfig_HidesAPIKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPIWithHTTP())

	resp, body := apiGet(t, srv.URL+"/api/v1/admin/config", testAdminAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: got %d", resp.StatusCode)
	}

	cfg := body["config"].(map[string]any)
	httpCfg := cfg["http"].(map[string]any)

	if httpCfg["admin_api_key"] != "[REDACTED]" {
		t.Errorf("admin_api_key not redacted: got %v", httpCfg["admin_api_key"])
	}
	if httpCfg["monitor_api_key"] != "[REDACTED]" {
		t.Errorf("monitor_api_key not redacted: got %v", httpCfg["monitor_api_key"])
	}

	// Double-check the actual secret never appears in the full response.
	raw := fmt.Sprintf("%v", body)
	if strings.Contains(raw, "real-secret-admin-key-1234") {
		t.Error("real admin API key leaked in response")
	}
}

// TestConfigAPI_Redaction_PatchPreview_HidesAPIKeys validates that
// PATCH response previews also redact API keys.
func TestConfigAPI_Redaction_PatchPreview_HidesAPIKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPIWithHTTP())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	body := applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "debug"},
	})

	preview := body["preview"].(map[string]any)
	httpCfg := preview["http"].(map[string]any)
	if httpCfg["admin_api_key"] != "[REDACTED]" {
		t.Errorf("admin_api_key not redacted in preview: got %v", httpCfg["admin_api_key"])
	}
}

// TestConfigAPI_TTL_MaxClampedTo30m validates that a TTL exceeding the
// maximum is clamped to 30 minutes.
func TestConfigAPI_TTL_MaxClampedTo30m(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	resp, body := apiPost(t, srv.URL+"/api/v1/admin/config/transactions", testAdminAPIKey, map[string]string{"ttl": "1h"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d", resp.StatusCode)
	}

	createdAt, _ := time.Parse(time.RFC3339, body["created_at"].(string))
	expiresAt, _ := time.Parse(time.RFC3339, body["expires_at"].(string))
	diff := expiresAt.Sub(createdAt)

	// Should be clamped to 30 minutes, not 1 hour.
	if diff > 31*time.Minute {
		t.Errorf("TTL not clamped: got %v, want <= 30m", diff)
	}
}

package integration_test

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
)

// ===============================================================
// Group 3: Config API Validation Feedback
//
// Validates that invalid config overlays produce proper 422
// responses with structured validation errors, and that the
// transaction is not poisoned by a rejected patch.
// ===============================================================

// TestConfigAPI_Patch_InvalidRouteRef_Returns422 validates that a patch
// referencing a nonexistent receiver_id returns 422 with validation errors.
func TestConfigAPI_Patch_InvalidRouteRef_Returns422(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	overlay := map[string]any{
		"routes": []map[string]any{
			{"id": "bad-route", "receiver_id": "nonexistent", "bindings": []string{"bind-1"}},
		},
	}
	url := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", srv.URL, txnID)
	resp, body := apiPatch(t, url, testAdminAPIKey, overlay)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422, body: %v", resp.StatusCode, body)
	}
	if body["error"] == nil {
		t.Error("response missing 'error' field")
	}
	errs, ok := body["validation_errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Error("response missing 'validation_errors' or empty")
	}
}

// TestConfigAPI_Patch_MissingBindingRef_Returns422 validates that a
// route referencing an undefined binding is rejected.
func TestConfigAPI_Patch_MissingBindingRef_Returns422(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	overlay := map[string]any{
		"routes": []map[string]any{
			{"id": "route-bad-bind", "receiver_id": "rx-1", "bindings": []string{"nonexistent-binding"}},
		},
	}
	url := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", srv.URL, txnID)
	resp, body := apiPatch(t, url, testAdminAPIKey, overlay)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422, body: %v", resp.StatusCode, body)
	}
}

// TestConfigAPI_Patch_InvalidDeliveryMode_Returns422 validates that an
// invalid delivery_mode value is rejected.
func TestConfigAPI_Patch_InvalidDeliveryMode_Returns422(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	overlay := map[string]any{
		"routes": []map[string]any{
			{
				"id":            "r1",
				"receiver_id":   "rx-1",
				"delivery_mode": "invalid_mode",
				"bindings":      []string{"bind-1"},
			},
		},
	}
	url := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", srv.URL, txnID)
	resp, _ := apiPatch(t, url, testAdminAPIKey, overlay)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", resp.StatusCode)
	}
}

// TestConfigAPI_Patch_InvalidJSON_Returns400 validates that a non-JSON
// body produces a 400 error.
func TestConfigAPI_Patch_InvalidJSON_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	url := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", srv.URL, txnID)
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader([]byte("not json{{")))
	req.Header.Set("X-API-Key", testAdminAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
}

// TestConfigAPI_Patch_AfterInvalidPatch_TransactionRemains validates that
// a rejected patch does not poison the transaction — subsequent valid
// patches still succeed.
func TestConfigAPI_Patch_AfterInvalidPatch_TransactionRemains(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	// Invalid patch: bad receiver reference.
	badOverlay := map[string]any{
		"routes": []map[string]any{
			{"id": "bad", "receiver_id": "missing", "bindings": []string{"bind-1"}},
		},
	}
	badURL := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", srv.URL, txnID)
	badResp, _ := apiPatch(t, badURL, testAdminAPIKey, badOverlay)
	if badResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad patch: got %d, want 422", badResp.StatusCode)
	}

	// Valid patch should still succeed.
	goodOverlay := map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "debug"},
	}
	goodResp, _ := apiPatch(t, badURL, testAdminAPIKey, goodOverlay)
	if goodResp.StatusCode != http.StatusOK {
		t.Fatalf("good patch after bad: got %d, want 200", goodResp.StatusCode)
	}
}

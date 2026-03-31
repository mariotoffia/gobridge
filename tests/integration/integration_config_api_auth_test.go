package integration_test

import (
	"fmt"
	"net/http"
	"testing"
)

// ===============================================================
// Group 2: Config API Auth Enforcement
//
// Validates that all config management endpoints require valid
// authentication over real HTTP, testing both X-API-Key and
// Bearer token styles, plus correlation header propagation.
// ===============================================================

// TestConfigAPI_Auth_NoKey_Returns401 validates that all config endpoints
// reject requests without an API key.
func TestConfigAPI_Auth_NoKey_Returns401(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/config"},
		{http.MethodPost, "/api/v1/admin/config/transactions"},
		{http.MethodGet, "/api/v1/admin/config/transactions/fake-id"},
		{http.MethodPatch, "/api/v1/admin/config/transactions/fake-id"},
		{http.MethodPost, "/api/v1/admin/config/transactions/fake-id/commit"},
		{http.MethodDelete, "/api/v1/admin/config/transactions/fake-id"},
	}

	for _, ep := range endpoints {
		t.Run(fmt.Sprintf("%s_%s", ep.method, ep.path), func(t *testing.T) {
			req, _ := http.NewRequest(ep.method, srv.URL+ep.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("missing WWW-Authenticate header")
			}
		})
	}
}

// TestConfigAPI_Auth_WrongKey_Returns401 validates that all config
// endpoints reject requests with an incorrect API key.
func TestConfigAPI_Auth_WrongKey_Returns401(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/config"},
		{http.MethodPost, "/api/v1/admin/config/transactions"},
	}

	for _, ep := range endpoints {
		t.Run(fmt.Sprintf("%s_%s", ep.method, ep.path), func(t *testing.T) {
			req, _ := http.NewRequest(ep.method, srv.URL+ep.path, nil)
			req.Header.Set("X-API-Key", testWrongAPIKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", resp.StatusCode)
			}
		})
	}
}

// TestConfigAPI_Auth_ValidXAPIKey_Succeeds validates that the X-API-Key
// header is accepted for authentication.
func TestConfigAPI_Auth_ValidXAPIKey_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/admin/config", nil)
	req.Header.Set("X-API-Key", testAdminAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("X-API-Key auth: got %d, want 200", resp.StatusCode)
	}
}

// TestConfigAPI_Auth_ValidBearerToken_Succeeds validates that the
// Authorization: Bearer header is accepted.
func TestConfigAPI_Auth_ValidBearerToken_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/admin/config", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Bearer auth: got %d, want 200", resp.StatusCode)
	}
}

// TestConfigAPI_Auth_CorrelationHeaders_Returned validates that
// correlation headers are present in responses.
func TestConfigAPI_Auth_CorrelationHeaders_Returned(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/admin/config", nil)
	req.Header.Set("X-API-Key", testAdminAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if resp.Header.Get("X-Correlation-Id") == "" {
		t.Error("missing X-Correlation-Id header")
	}
}

// TestConfigAPI_Auth_CustomCorrelationID_Echoed validates that a
// client-provided correlation ID is echoed back.
func TestConfigAPI_Auth_CustomCorrelationID_Echoed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/admin/config", nil)
	req.Header.Set("X-API-Key", testAdminAPIKey)
	req.Header.Set("X-Correlation-Id", "my-custom-corr-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	got := resp.Header.Get("X-Correlation-Id")
	if got != "my-custom-corr-id" {
		t.Errorf("correlation ID: got %q, want my-custom-corr-id", got)
	}
}

// TestConfigAPI_SecurityHeaders_Present validates that security headers
// are set on config API responses.
func TestConfigAPI_SecurityHeaders_Present(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	resp, _ := apiGet(t, srv.URL+"/api/v1/admin/config", testAdminAPIKey)

	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":       "DENY",
		"Referrer-Policy":       "no-referrer",
	}
	for header, want := range checks {
		got := resp.Header.Get(header)
		if got != want {
			t.Errorf("%s: got %q, want %q", header, got, want)
		}
	}
}

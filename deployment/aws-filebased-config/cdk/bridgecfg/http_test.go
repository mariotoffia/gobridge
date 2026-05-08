package bridgecfg_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
)

func TestWithHTTPAdminAPI_PopulatesHTTPSection(t *testing.T) {
	opts := bridgecfg.AdminAPIDefaults()
	opts.AdminAPIKey = "pms://bridge/admin-key"
	opts.MonitorAddr = ":9090"
	opts.MonitorAPIKey = "pms://bridge/monitor-key"
	opts.CORSOrigins = "https://app.example.com"

	cfg, err := bridgecfg.New("b").
		WithHTTPAdminAPI(opts).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg.HTTP == nil {
		t.Fatal("HTTP block is nil")
	}
	if cfg.HTTP.AdminAddr != ":8080" {
		t.Errorf("AdminAddr = %q, want :8080", cfg.HTTP.AdminAddr)
	}
	if cfg.HTTP.MonitorAddr != ":9090" {
		t.Errorf("MonitorAddr = %q, want :9090", cfg.HTTP.MonitorAddr)
	}
	if cfg.HTTP.AdminAPIKey != "pms://bridge/admin-key" {
		t.Errorf("AdminAPIKey = %q, want pms://bridge/admin-key", cfg.HTTP.AdminAPIKey)
	}
	if cfg.HTTP.MonitorAPIKey != "pms://bridge/monitor-key" {
		t.Errorf("MonitorAPIKey = %q, want pms://bridge/monitor-key", cfg.HTTP.MonitorAPIKey)
	}
	if cfg.HTTP.CORSOrigins != "https://app.example.com" {
		t.Errorf("CORSOrigins = %q, want https://app.example.com", cfg.HTTP.CORSOrigins)
	}
}

func TestWithHTTPAdminAPI_PlaintextKeyRejected(t *testing.T) {
	opts := bridgecfg.AdminAPIDefaults()
	opts.AdminAPIKey = "literal-secret"
	_, err := bridgecfg.New("b").
		WithHTTPAdminAPI(opts).
		Build()
	if err == nil {
		t.Fatal("expected plaintext-secret error for inline AdminAPIKey")
	}
	if !strings.Contains(err.Error(), "plaintext secret") {
		t.Errorf("error = %v, want one mentioning plaintext secret", err)
	}
	if !strings.Contains(err.Error(), "http.admin_api_key") {
		t.Errorf("error = %v, want one naming http.admin_api_key", err)
	}
}

func TestWithHTTPAdminAPI_LastCallWins(t *testing.T) {
	first := bridgecfg.AdminAPIDefaults()
	first.AdminAPIKey = "pms://first"
	second := bridgecfg.AdminAPIDefaults()
	second.AdminAddr = ":7070"
	second.AdminAPIKey = "pms://second"

	cfg, err := bridgecfg.New("b").
		WithHTTPAdminAPI(first).
		WithHTTPAdminAPI(second).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg.HTTP.AdminAddr != ":7070" {
		t.Errorf("AdminAddr = %q, want :7070 (second call wins)", cfg.HTTP.AdminAddr)
	}
	if cfg.HTTP.AdminAPIKey != "pms://second" {
		t.Errorf("AdminAPIKey = %q, want pms://second", cfg.HTTP.AdminAPIKey)
	}
}

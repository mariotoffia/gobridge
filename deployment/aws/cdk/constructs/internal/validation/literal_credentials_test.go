package validation

import (
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/source"
)

func TestPhase1MaterializedLiteralKeysArePreserved(t *testing.T) {
	options := bridgecfg.AdminAPIDefaults()
	options.AdminAPIKey = "consumer-admin-key-0123456789"
	options.MonitorAPIKey = "consumer-monitor-key-0123456789"
	cfg, err := bridgecfg.New(validBridgeID).WithHTTPAdminAPI(options).Build()
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	materialized, err := source.NewInline(cfg).Materialize()
	if err != nil {
		t.Fatalf("materialize config: %v", err)
	}
	in := baseInput(materialized.Config)
	in.Materialized = materialized
	if err := Phase1(in); err != nil {
		t.Fatalf("Phase1 rejected literal keys: %v", err)
	}
	if materialized.Config.HTTP.AdminAPIKey.Reveal() != options.AdminAPIKey ||
		materialized.Config.HTTP.MonitorAPIKey.Reveal() != options.MonitorAPIKey {
		t.Fatal("Phase1 changed the selected credentials")
	}
}

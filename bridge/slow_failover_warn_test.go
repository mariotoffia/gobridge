package bridge

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

func TestClusteredLeaseCadenceDoesNotDeclareFailoverSLO(t *testing.T) {
	cfg, err := toSessionConfigE(&ports.RouteSessionDef{SessionID: "exclusive"}, true)
	if err != nil {
		t.Fatalf("clustered lease cadence: %v", err)
	}
	if cfg.LeaseTTL != 45*time.Second {
		t.Fatalf("clustered lease TTL=%s", cfg.LeaseTTL)
	}
	if cfg.FailoverSLO != 0 {
		t.Fatalf("clustered cadence declared failover SLO=%s", cfg.FailoverSLO)
	}
}

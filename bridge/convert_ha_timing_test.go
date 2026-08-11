package bridge

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// TestToSessionConfig_ClusteredHATiming proves that a clustered, lease-bearing
// exclusive session with no explicit lease timing derives the fast-failover HA
// baseline (LeaseTTL=45s) instead of DefaultConfig's ~6-minute LeaseTTL, that explicit operator timing wins, and that a non-clustered session
// keeps the DefaultConfig baseline. Reverting the HA wiring makes the clustered
// case fall back to 360s and fail.
func TestToSessionConfig_ClusteredHATiming(t *testing.T) {
	haTTL := session.HAConfig("x", true).LeaseTTL
	defTTL := session.DefaultConfig("x", true).LeaseTTL

	tests := []struct {
		name      string
		clustered bool
		def       *ports.RouteSessionDef
		wantTTL   time.Duration
	}{
		{
			name:      "clustered, no explicit timing -> HA baseline",
			clustered: true,
			def:       &ports.RouteSessionDef{SessionID: "s1"},
			wantTTL:   haTTL, // 45s
		},
		{
			name:      "clustered, explicit lease_ttl wins",
			clustered: true,
			def:       &ports.RouteSessionDef{SessionID: "s1", LeaseTTL: "200s"},
			wantTTL:   200 * time.Second,
		},
		{
			name:      "clustered, explicit renew_interval keeps default TTL baseline",
			clustered: true,
			def:       &ports.RouteSessionDef{SessionID: "s1", RenewInterval: "20s"},
			wantTTL:   defTTL, // renew_interval set => not the HA path
		},
		{
			name:      "non-clustered, no explicit timing -> default baseline",
			clustered: false,
			def:       &ports.RouteSessionDef{SessionID: "s1"},
			wantTTL:   defTTL, // 360s
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc, err := toSessionConfigE(tt.def, tt.clustered)
			if err != nil {
				t.Fatalf("toSessionConfigE: %v", err)
			}
			if sc == nil {
				t.Fatal("expected non-nil session config")
			}
			if sc.LeaseTTL != tt.wantTTL {
				t.Fatalf("LeaseTTL = %s, want %s", sc.LeaseTTL, tt.wantTTL)
			}
		})
	}
}

// TestToSessionConfig_ClusteredHATiming_PinsRenewCadence proves the HA path
// carries HAConfig's internally-consistent renew cadence (RenewInterval=10s,
// RenewCallTimeout=3s, StepDownGrace=5s) verbatim, not DefaultConfig's, so the
// worst-case renew span stays under the 45s TTL.
func TestToSessionConfig_ClusteredHATiming_PinsRenewCadence(t *testing.T) {
	ha := session.HAConfig("s1", true)
	sc, err := toSessionConfigE(&ports.RouteSessionDef{SessionID: "s1"}, true)
	if err != nil {
		t.Fatalf("toSessionConfigE: %v", err)
	}
	if sc.RenewInterval != ha.RenewInterval {
		t.Fatalf("RenewInterval = %s, want HA %s", sc.RenewInterval, ha.RenewInterval)
	}
	if sc.RenewCallTimeout != ha.RenewCallTimeout {
		t.Fatalf("RenewCallTimeout = %s, want HA %s", sc.RenewCallTimeout, ha.RenewCallTimeout)
	}
	if sc.StepDownGrace != ha.StepDownGrace {
		t.Fatalf("StepDownGrace = %s, want HA %s", sc.StepDownGrace, ha.StepDownGrace)
	}
}

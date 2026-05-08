//go:build !race

package gobridgebase_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

func TestDerivePortMappings(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ports.BridgeConfig
		boot infra.BootstrapConfig
		want map[gobridgebase.PortKind]float64
	}{
		{
			name: "defaults-no-http-receiver-no-transport",
			cfg:  &ports.BridgeConfig{},
			boot: infra.BootstrapConfig{},
			want: map[gobridgebase.PortKind]float64{
				gobridgebase.PortKindAdmin:   8080,
				gobridgebase.PortKindMonitor: 8081,
			},
		},
		{
			name: "with-http-receiver-emits-transport",
			cfg: &ports.BridgeConfig{
				Receivers: []ports.ReceiverDef{{ID: "r1", Transport: "http"}},
			},
			boot: infra.BootstrapConfig{},
			want: map[gobridgebase.PortKind]float64{
				gobridgebase.PortKindAdmin:     8080,
				gobridgebase.PortKindMonitor:   8081,
				gobridgebase.PortKindTransport: 8082,
			},
		},
		{
			name: "yaml-overrides-bootstrap-admin-monitor",
			cfg: &ports.BridgeConfig{
				HTTP: &ports.HTTPConfig{AdminAddr: ":9090", MonitorAddr: ":9091"},
			},
			boot: infra.BootstrapConfig{},
			want: map[gobridgebase.PortKind]float64{
				gobridgebase.PortKindAdmin:   9090,
				gobridgebase.PortKindMonitor: 9091,
			},
		},
		{
			name: "monitor-empty-skipped",
			cfg:  &ports.BridgeConfig{},
			boot: infra.BootstrapConfig{
				AdminAddr:   ":7000",
				MonitorAddr: " ", // technically non-empty; portFromAddr rejects it
			},
			want: map[gobridgebase.PortKind]float64{
				gobridgebase.PortKindAdmin: 7000,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gobridgebase.DerivePortMappings(tc.cfg, tc.boot)
			gotMap := map[gobridgebase.PortKind]float64{}
			for _, m := range got {
				gotMap[m.Kind] = m.Port
			}
			if len(gotMap) != len(tc.want) {
				t.Fatalf("got %d mappings (%v), want %d (%v)", len(gotMap), gotMap, len(tc.want), tc.want)
			}
			for k, v := range tc.want {
				if gotMap[k] != v {
					t.Fatalf("kind %s: got %v want %v", k, gotMap[k], v)
				}
			}
		})
	}
}

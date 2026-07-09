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
			// c15-cdk-ports: the file-based runtime IGNORES the bridge
			// yaml `http:` block (lib/bootstrap.checkIgnoredHTTPBlock) and
			// binds admin/monitor to the BootstrapConfig addresses only.
			// The derived ports MUST therefore ignore http.admin_addr /
			// http.monitor_addr and stay on the bootstrap defaults (8080 /
			// 8081) — otherwise target groups aim at ports nothing listens
			// on. Mutation: repoint DerivePortMappings back at cfg.HTTP →
			// these ports flip to 9090/9091 and this case FAILs.
			name: "http-block-does-not-sway-ports",
			cfg: &ports.BridgeConfig{
				HTTP: &ports.HTTPConfig{AdminAddr: ":9090", MonitorAddr: ":9091"},
			},
			boot: infra.BootstrapConfig{},
			want: map[gobridgebase.PortKind]float64{
				gobridgebase.PortKindAdmin:   8080,
				gobridgebase.PortKindMonitor: 8081,
			},
		},
		{
			// Ports follow the bootstrap listen addresses the runtime
			// actually binds, even when the (ignored) http: block sets
			// conflicting values. Bootstrap is the single authority.
			name: "bootstrap-addrs-win-over-http-block",
			cfg: &ports.BridgeConfig{
				HTTP: &ports.HTTPConfig{AdminAddr: ":9090", MonitorAddr: ":9091"},
			},
			boot: infra.BootstrapConfig{
				AdminAddr:   ":7000",
				MonitorAddr: ":7001",
			},
			want: map[gobridgebase.PortKind]float64{
				gobridgebase.PortKindAdmin:   7000,
				gobridgebase.PortKindMonitor: 7001,
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

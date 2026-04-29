package bridge

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

func TestToRoutePolicy_InvalidBackoffDuration_ReturnsError(t *testing.T) {
	tests := []struct {
		name string
		def  ports.RouteDef
	}{
		{
			name: "invalid initial interval",
			def: ports.RouteDef{
				Policy: ports.PolicyDef{
					Backoff: ports.BackoffDef{InitialInterval: "5x"},
				},
			},
		},
		{
			name: "invalid max interval",
			def: ports.RouteDef{
				Policy: ports.PolicyDef{
					Backoff: ports.BackoffDef{MaxInterval: "abc"},
				},
			},
		},
		{
			name: "both invalid",
			def: ports.RouteDef{
				Policy: ports.PolicyDef{
					Backoff: ports.BackoffDef{
						InitialInterval: "nope",
						MaxInterval:     "bad",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := toRoutePolicyE(tt.def)
			if err == nil {
				t.Fatal("expected error for invalid duration string, got nil")
			}
		})
	}
}

func TestToSessionConfig_InvalidDuration_ReturnsError(t *testing.T) {
	tests := []struct {
		name string
		def  *ports.RouteSessionDef
	}{
		{
			name: "invalid lease TTL",
			def:  &ports.RouteSessionDef{SessionID: "s1", LeaseTTL: "bad"},
		},
		{
			name: "invalid renew interval",
			def:  &ports.RouteSessionDef{SessionID: "s1", RenewInterval: "xyz"},
		},
		{
			name: "invalid step-down grace",
			def:  &ports.RouteSessionDef{SessionID: "s1", StepDownGrace: "nope"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := toSessionConfigE(tt.def)
			if err == nil {
				t.Fatal("expected error for invalid duration string, got nil")
			}
		})
	}
}

func TestBuildDrainStrategy_InvalidDuration_ReturnsError(t *testing.T) {
	tests := []struct {
		name string
		def  *ports.DrainStrategyDef
	}{
		{
			name: "invalid fixed poll interval",
			def:  &ports.DrainStrategyDef{Type: "fixed_poll", Interval: "bad"},
		},
		{
			name: "invalid adaptive min interval",
			def: &ports.DrainStrategyDef{
				Type:        "adaptive_backoff",
				MinInterval: "nope",
				MaxInterval: "10s",
			},
		},
		{
			name: "invalid adaptive max interval",
			def: &ports.DrainStrategyDef{
				Type:        "adaptive_backoff",
				MinInterval: "1s",
				MaxInterval: "bad",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildDrainStrategyE(tt.def)
			if err == nil {
				t.Fatal("expected error for invalid duration string, got nil")
			}
		})
	}
}

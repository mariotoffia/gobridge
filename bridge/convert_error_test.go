package bridge

import (
	"testing"

	"github.com/mariotoffia/gobridge/config"
)

func TestToRoutePolicy_InvalidBackoffDuration_ReturnsError(t *testing.T) {
	tests := []struct {
		name string
		def  config.RouteDef
	}{
		{
			name: "invalid initial interval",
			def: config.RouteDef{
				Policy: config.PolicyDef{
					Backoff: config.BackoffDef{InitialInterval: "5x"},
				},
			},
		},
		{
			name: "invalid max interval",
			def: config.RouteDef{
				Policy: config.PolicyDef{
					Backoff: config.BackoffDef{MaxInterval: "abc"},
				},
			},
		},
		{
			name: "both invalid",
			def: config.RouteDef{
				Policy: config.PolicyDef{
					Backoff: config.BackoffDef{
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
		def  *config.RouteSessionDef
	}{
		{
			name: "invalid lease TTL",
			def:  &config.RouteSessionDef{SessionID: "s1", LeaseTTL: "bad"},
		},
		{
			name: "invalid renew interval",
			def:  &config.RouteSessionDef{SessionID: "s1", RenewInterval: "xyz"},
		},
		{
			name: "invalid step-down grace",
			def:  &config.RouteSessionDef{SessionID: "s1", StepDownGrace: "nope"},
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
		def  *config.DrainStrategyDef
	}{
		{
			name: "invalid fixed poll interval",
			def:  &config.DrainStrategyDef{Type: "fixed_poll", Interval: "bad"},
		},
		{
			name: "invalid adaptive min interval",
			def: &config.DrainStrategyDef{
				Type:        "adaptive_backoff",
				MinInterval: "nope",
				MaxInterval: "10s",
			},
		},
		{
			name: "invalid adaptive max interval",
			def: &config.DrainStrategyDef{
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

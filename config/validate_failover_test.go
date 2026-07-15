package config

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

func TestValidateFailoverFieldsDeclaredUnitsAndBounds(t *testing.T) {
	cases := map[string]struct {
		slo, startup string
		wantErrors   int
	}{
		"undeclared":        {"", "", 0},
		"valid":             {"60s", "0s", 0},
		"zero-slo":          {"0s", "", 1},
		"negative-slo":      {"-1ns", "", 1},
		"malformed-slo":     {"soon", "", 1},
		"negative-startup":  {"60s", "-1ns", 1},
		"unbounded-startup": {"20m", "10m0.000000001s", 1},
		"overflow":          {"2562048h", "", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ve := &ValidationError{}
			validateFailoverFields(ve, &ports.BridgeConfig{Routes: []ports.RouteDef{{ID: "r", Session: &ports.RouteSessionDef{FailoverSLO: tc.slo, StartupAllowance: tc.startup}}}})
			if len(ve.Errors) != tc.wantErrors {
				t.Fatalf("errors=%v", ve.Errors)
			}
		})
	}
}

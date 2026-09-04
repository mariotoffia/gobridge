package validate_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// The admin config transaction validates, writes DURABLY, and only then
// applies. The broker-path decision therefore has to be judged here too, or an
// objective that excludes a whole failure mode commits and fails at apply.
func TestValidateBlueprintGraph_DeclaredFailoverSLORequiresABrokerPathDecision(t *testing.T) {
	cfg := sessionRouteConfig()
	cfg.Routes[0].Session.FailoverSLO = "120s"
	got := errorString(t, cfg)
	if !strings.Contains(got, "broker_health_step_down") ||
		!strings.Contains(got, routing.BrokerPathFailoverOff) {
		t.Fatalf("an undeclared broker-path policy under a declared failover_slo must fail before commit, got: %q", got)
	}
}

func TestValidateBlueprintGraph_BrokerPathDecisionAcceptsExplicitOffAndDuration(t *testing.T) {
	for _, value := range []string{routing.BrokerPathFailoverOff, "45s"} {
		t.Run(value, func(t *testing.T) {
			cfg := sessionRouteConfig()
			cfg.Routes[0].Session.FailoverSLO = "120s"
			cfg.Routes[0].Session.BrokerHealthStepDown = value
			if got := errorString(t, cfg); got != "" {
				t.Fatalf("broker_health_step_down=%q rejected: %s", value, got)
			}
		})
	}
}

func TestValidateBlueprintGraph_UndeclaredFailoverSLONeedsNoBrokerPathDecision(t *testing.T) {
	if got := errorString(t, sessionRouteConfig()); got != "" {
		t.Fatalf("no objective declared: %s", got)
	}
}

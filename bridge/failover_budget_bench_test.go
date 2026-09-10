package bridge

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// Failover admission runs once per route/binding manager candidate on every
// build and on every live config apply, so its cost sits on the reload path an
// operator waits on. Admitting a declared objective now evaluates TWO formulas
// rather than one; these baselines are what a later change to either is
// measured against.

func benchFailoverCandidate(brokerHealthStepDown time.Duration) failoverManagerCandidate {
	cfg := session.HAConfig("bench", true)
	cfg.FailoverSLO = 20 * time.Minute
	cfg.StartupAllowance = 10 * time.Second
	cfg.BrokerHealthStepDown = brokerHealthStepDown
	cfg.BrokerPathFailoverDeclared = true
	return failoverManagerCandidate{
		sessionID: "bench", source: `route "bench"`, config: cfg,
		inputs: failoverManagerInputs{postTakeoverActivation: 240 * time.Second},
	}
}

// BenchmarkAdmitFailoverBudgets contrasts the two admissions an operator can
// choose between: owner death alone, and owner death plus the broker path.
func BenchmarkAdmitFailoverBudgets(b *testing.B) {
	for _, tc := range []struct {
		name     string
		stepDown time.Duration
	}{
		{name: "owner_death_only", stepDown: 0},
		{name: "with_broker_path", stepDown: 90 * time.Second},
	} {
		b.Run(tc.name, func(b *testing.B) {
			candidate := benchFailoverCandidate(tc.stepDown)
			b.ReportAllocs()
			for b.Loop() {
				if err := admitFailoverBudgets(candidate, "bench"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCheckedBrokerPathFailoverBudget isolates the new formula from the
// config resolution around it.
func BenchmarkCheckedBrokerPathFailoverBudget(b *testing.B) {
	in := brokerPathBudgetFor(benchFailoverCandidate(90*time.Second).config, 240*time.Second)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := checkedBrokerPathFailoverBudget(in); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuilderPlan_FailoverAdmission is the whole-blueprint path: every
// route session resolved, canonicalized and admitted. It is the number that
// moves if admission ever becomes visible in reload latency.
func BenchmarkBuilderPlan_FailoverAdmission(b *testing.B) {
	for _, tc := range []struct {
		name     string
		stepDown string
	}{
		{name: "broker_path_off", stepDown: routing.BrokerPathFailoverOff},
		{name: "broker_path_on", stepDown: "7s"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			cfg := failoverBudgetBlueprint("27s", failoverTimingPluginConfig{
				timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second},
			})
			cfg.Routes[0].Session.BrokerHealthStepDown = tc.stepDown
			builder := NewBuilder(cfg)
			b.ReportAllocs()
			for b.Loop() {
				if err := builder.validateFailoverBudgets(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

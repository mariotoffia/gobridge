package bridge

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// with NO failover_slo declared, the worst-case failover budget was
// previously computed nowhere — an operator selecting the clustered HA
// profile could assume ~lease-TTL failover while the real worst case is
// minutes. Every build must now DISCLOSE the computed budget for exclusive
// sessions so the number is on record before an incident.
func TestBuilderPlan_UndeclaredFailoverSLODisclosesComputedBudget(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := failoverBudgetBlueprint("", failoverTimingPluginConfig{
		timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second},
	})

	plan, err := NewBuilder(cfg, WithLogger(logger)).Plan(t.Context())
	if err != nil {
		t.Fatalf("undeclared SLO must not fail the build: %v", err)
	}
	plan.Close()

	logged := buf.String()
	if !strings.Contains(logged, "worst-case failover budget") {
		t.Fatalf("expected failover budget disclosure log, got: %s", logged)
	}
	// The blueprint's budget: TTL 5s + 2*poll(5s) + 3 calls*1s + activation 5s
	// + startup 4s = 27s (same arithmetic the enforced-SLO tests pin).
	if !strings.Contains(logged, "failover_budget=27s") {
		t.Fatalf("expected the computed 27s budget in the disclosure, got: %s", logged)
	}
	if !strings.Contains(logged, "failover_slo") {
		t.Fatalf("disclosure must point the operator at failover_slo, got: %s", logged)
	}
}

// The disclosure is best-effort: a transport with no timing capability logs
// the unknown-budget line and never fails the build (that enforcement is
// exactly what declaring failover_slo buys).
func TestBuilderPlan_UndeclaredFailoverSLOUnknownTimingDisclosesUnknown(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	plan, err := NewBuilder(
		failoverBudgetBlueprint("", failoverNoTimingPluginConfig{}),
		WithLogger(logger),
	).Plan(t.Context())
	if err != nil {
		t.Fatalf("unknown timing without SLO must not fail the build: %v", err)
	}
	plan.Close()

	if !strings.Contains(buf.String(), "failover budget for exclusive session is UNKNOWN") {
		t.Fatalf("expected unknown-budget disclosure, got: %s", buf.String())
	}
}

// A disclosure that names only the owner-death budget invites the same wrong
// assumption the disclosure exists to prevent — that the number covers every
// way this session can fail over. It must say where broker-path failover
// stands, and give its budget when it is on.
func TestBuilderPlan_DisclosureNamesTheBrokerPathDecision(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stepDown   string
		wantInLog  string
		wantBudget string
	}{
		{name: "undeclared", stepDown: "", wantInLog: "broker_path_failover=undeclared"},
		{name: "explicitly off", stepDown: "off", wantInLog: "broker_path_failover=off"},
		{name: "enabled", stepDown: "7s", wantInLog: "broker_path_failover=7s", wantBudget: "broker_path_failover_budget=34s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			cfg := failoverBudgetBlueprint("", failoverTimingPluginConfig{
				timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second},
			})
			cfg.Routes[0].Session.BrokerHealthStepDown = tc.stepDown

			plan, err := NewBuilder(cfg, WithLogger(logger)).Plan(t.Context())
			if err != nil {
				t.Fatalf("undeclared SLO must not fail the build: %v", err)
			}
			plan.Close()

			logged := buf.String()
			if !strings.Contains(logged, tc.wantInLog) {
				t.Fatalf("disclosure must state the broker-path decision (%s), got: %s", tc.wantInLog, logged)
			}
			if tc.wantBudget != "" && !strings.Contains(logged, tc.wantBudget) {
				t.Fatalf("disclosure must carry the broker-path budget (%s), got: %s", tc.wantBudget, logged)
			}
		})
	}
}

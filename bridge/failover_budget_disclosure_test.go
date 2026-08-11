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

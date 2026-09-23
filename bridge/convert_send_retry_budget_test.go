package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// send_retry_budget is tri-state on the wire: omitted takes the default, an
// explicit zero turns in-process send retry off, and a positive duration is the
// budget. The defaulted policy is what the route runs, so each case is checked
// after WithDefaults as well.
func TestToRoutePolicy_SendRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name          string
		wire          string
		wantParsed    time.Duration
		wantEffective time.Duration
	}{
		{name: "omitted takes the default", wire: "", wantParsed: 0, wantEffective: routing.DefaultSendRetryBudget},
		{name: "explicit zero disables", wire: "0s", wantParsed: routing.SendRetryBudgetDisabled,
			wantEffective: routing.SendRetryBudgetDisabled},
		{name: "bare zero disables", wire: "0", wantParsed: routing.SendRetryBudgetDisabled,
			wantEffective: routing.SendRetryBudgetDisabled},
		{name: "positive is the budget", wire: "45s", wantParsed: 45 * time.Second, wantEffective: 45 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := toRoutePolicyE(ports.RouteDef{Policy: ports.PolicyDef{SendRetryBudget: tc.wire}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.SendRetryBudget != tc.wantParsed {
				t.Fatalf("parsed SendRetryBudget = %v, want %v", p.SendRetryBudget, tc.wantParsed)
			}
			if got := p.WithDefaults().SendRetryBudget; got != tc.wantEffective {
				t.Fatalf("effective SendRetryBudget = %v, want %v", got, tc.wantEffective)
			}
		})
	}
}

// A negative or unparseable budget is refused at the parse boundary, naming the
// wire key, so a route built through the library API cannot carry a value the
// config path would reject.
func TestToRoutePolicy_InvalidSendRetryBudget(t *testing.T) {
	for _, wire := range []string{"-1s", "abc"} {
		t.Run(wire, func(t *testing.T) {
			_, err := toRoutePolicyE(ports.RouteDef{Policy: ports.PolicyDef{SendRetryBudget: wire}})
			if err == nil {
				t.Fatalf("send_retry_budget %q must be rejected", wire)
			}
			if !strings.Contains(err.Error(), "send_retry_budget") {
				t.Fatalf("error should name the send_retry_budget field; got %v", err)
			}
		})
	}
}

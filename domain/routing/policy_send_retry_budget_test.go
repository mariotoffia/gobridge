package routing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// An unset send retry budget takes the 60s default, so a route loaded from a
// blueprint that never mentions send_retry_budget still rides out a short
// destination outage instead of dead-lettering on the first failed send.
func TestRoutePolicy_WithDefaults_SendRetryBudgetUnsetTakesDefault(t *testing.T) {
	p := routing.RoutePolicy{}.WithDefaults()
	if p.SendRetryBudget != routing.DefaultSendRetryBudget {
		t.Fatalf("SendRetryBudget = %v, want %v", p.SendRetryBudget, routing.DefaultSendRetryBudget)
	}
	if routing.DefaultSendRetryBudget != 60*time.Second {
		t.Fatalf("DefaultSendRetryBudget = %v, want 60s", routing.DefaultSendRetryBudget)
	}
}

// The budget is tri-state like jitter: SendRetryBudgetDisabled is an operator
// turning in-process retry off, and defaulting must not turn it back on.
func TestRoutePolicy_WithDefaults_SendRetryBudgetDisabledSurvives(t *testing.T) {
	p := routing.RoutePolicy{SendRetryBudget: routing.SendRetryBudgetDisabled}.WithDefaults()
	if p.SendRetryBudget != routing.SendRetryBudgetDisabled {
		t.Fatalf("SendRetryBudget = %v, want SendRetryBudgetDisabled; defaulting re-enabled a deliberate opt-out",
			p.SendRetryBudget)
	}
	if p.SendRetryBudget > 0 {
		t.Fatal("SendRetryBudgetDisabled must read as 'no in-process retry' (<= 0)")
	}
}

func TestRoutePolicy_WithDefaults_SendRetryBudgetExplicitPreserved(t *testing.T) {
	p := routing.RoutePolicy{SendRetryBudget: 30 * time.Second}.WithDefaults()
	if p.SendRetryBudget != 30*time.Second {
		t.Fatalf("SendRetryBudget = %v, want the explicit 30s", p.SendRetryBudget)
	}
}

// Validate accepts the unset zero and the explicit opt-out, and rejects every
// other negative budget as a permanent configuration defect.
func TestRoutePolicy_Validate_SendRetryBudget(t *testing.T) {
	tests := []struct {
		name    string
		budget  time.Duration
		wantErr bool
	}{
		{name: "negative rejected", budget: -5 * time.Second, wantErr: true},
		{name: "disabled sentinel allowed", budget: routing.SendRetryBudgetDisabled, wantErr: false},
		{name: "zero allowed (use default)", budget: 0, wantErr: false},
		{name: "positive allowed", budget: 45 * time.Second, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := routing.RoutePolicy{SendRetryBudget: tt.budget}.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var be *shared.BridgeError
			if !errors.As(err, &be) {
				t.Fatalf("want *shared.BridgeError for a negative SendRetryBudget, got %T (%v)", err, err)
			}
			if be.Class != shared.ErrorPermanent {
				t.Fatalf("error class = %v, want %v", be.Class, shared.ErrorPermanent)
			}
			if be.Code != shared.ErrCodeInvalidConfig {
				t.Fatalf("error code = %v, want %v", be.Code, shared.ErrCodeInvalidConfig)
			}
		})
	}
}

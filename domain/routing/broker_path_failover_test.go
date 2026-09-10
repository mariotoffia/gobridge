package routing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestParseBrokerPathPolicy_UnsetLeavesTheDecisionUndeclared(t *testing.T) {
	p, err := routing.ParseBrokerPathPolicy("")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if p.Declared || p.Enabled() || p.StepDown != 0 {
		t.Fatalf("empty policy=%+v want undeclared and disabled", p)
	}
}

func TestParseBrokerPathPolicy_OffIsAnExplicitDecisionToDisable(t *testing.T) {
	p, err := routing.ParseBrokerPathPolicy(routing.BrokerPathFailoverOff)
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	if !p.Declared {
		t.Fatal("off must count as a declared decision")
	}
	if p.Enabled() || p.StepDown != 0 {
		t.Fatalf("off policy=%+v want disabled", p)
	}
}

func TestParseBrokerPathPolicy_PositiveDurationEnablesAndDeclares(t *testing.T) {
	p, err := routing.ParseBrokerPathPolicy("90s")
	if err != nil {
		t.Fatalf("90s: %v", err)
	}
	if !p.Declared || !p.Enabled() || p.StepDown != 90*time.Second {
		t.Fatalf("policy=%+v want declared, enabled, 90s", p)
	}
}

func TestParseBrokerPathPolicy_RejectsUnparseableAndNonPositive(t *testing.T) {
	for _, raw := range []string{"nonsense", "0s", "-1s", "OFF"} {
		if _, err := routing.ParseBrokerPathPolicy(raw); !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("raw=%q err=%v want ErrInvalidConfig", raw, err)
		}
	}
}

func TestValidateBrokerPathPolicy_DeclaredObjectiveRequiresADecision(t *testing.T) {
	undeclared, err := routing.ParseBrokerPathPolicy("")
	if err != nil {
		t.Fatal(err)
	}
	err = routing.ValidateBrokerPathPolicy("session \"s\"", time.Minute, undeclared)
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("err=%v want ErrInvalidConfig", err)
	}
}

func TestValidateBrokerPathPolicy_UndeclaredObjectiveNeedsNoDecision(t *testing.T) {
	undeclared, err := routing.ParseBrokerPathPolicy("")
	if err != nil {
		t.Fatal(err)
	}
	if err := routing.ValidateBrokerPathPolicy("session \"s\"", 0, undeclared); err != nil {
		t.Fatalf("no objective declared: %v", err)
	}
}

func TestValidateBrokerPathPolicy_AcceptsEitherExplicitDecision(t *testing.T) {
	for _, raw := range []string{routing.BrokerPathFailoverOff, "90s"} {
		p, err := routing.ParseBrokerPathPolicy(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if err := routing.ValidateBrokerPathPolicy("session \"s\"", time.Minute, p); err != nil {
			t.Fatalf("%q rejected: %v", raw, err)
		}
	}
}

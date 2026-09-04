package session_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// A declared failover objective is a claim about how long a takeover can take.
// Leaving broker_health_step_down at its default silently excludes the
// node-local broker outage from that claim, so the decision must be stated.
func TestConfigValidate_DeclaredFailoverSLORequiresABrokerPathDecision(t *testing.T) {
	cfg := session.DefaultConfig("s", true)
	cfg.FailoverSLO = 20 * time.Minute
	err := cfg.Validate()
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("err=%v want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), routing.BrokerPathFailoverOff) {
		t.Fatalf("diagnostic must name the explicit opt-out spelling: %v", err)
	}
}

func TestConfigValidate_ExplicitBrokerPathDecisionsBothPass(t *testing.T) {
	enabled := session.DefaultConfig("s", true)
	enabled.FailoverSLO = 20 * time.Minute
	enabled.BrokerHealthStepDown = 90 * time.Second
	if err := enabled.Validate(); err != nil {
		t.Fatalf("enabled: %v", err)
	}

	off := session.DefaultConfig("s", true)
	off.FailoverSLO = 20 * time.Minute
	off.BrokerPathFailoverDeclared = true
	if err := off.Validate(); err != nil {
		t.Fatalf("explicit off: %v", err)
	}
}

func TestConfigValidate_UndeclaredFailoverSLONeedsNoBrokerPathDecision(t *testing.T) {
	cfg := session.DefaultConfig("s", true)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("no objective declared: %v", err)
	}
}

// A non-exclusive session never acquires a lease, so it has no failover to
// bound and no broker-path decision to make.
func TestConfigValidate_NonExclusiveSessionNeedsNoBrokerPathDecision(t *testing.T) {
	cfg := session.DefaultConfig("s", false)
	cfg.FailoverSLO = 20 * time.Minute
	if err := cfg.Validate(); err != nil {
		t.Fatalf("non-exclusive: %v", err)
	}
}

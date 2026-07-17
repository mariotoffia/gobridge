package bridge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

type activationTimingPluginConfig struct {
	timing ports.SessionActivationTiming
}

func (activationTimingPluginConfig) Kind() string                             { return "timing.test" }
func (activationTimingPluginConfig) Validate() error                          { return nil }
func (c activationTimingPluginConfig) FreezePluginConfig() ports.PluginConfig { return c }
func (c activationTimingPluginConfig) PostAcquireActivationTiming(connectivity.SessionMode) ports.SessionActivationTiming {
	return c.timing
}

func activationTimingBlueprint(leaseTTL, stepDownGrace string, timing ports.SessionActivationTiming) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "activation-timing"},
		Sessions: []ports.SessionDef{{
			ID: "exclusive-mqtt", Transport: "timing.test", SessionMode: string(connectivity.SessionExclusive),
			Config: activationTimingPluginConfig{timing: timing},
		}},
		Routes: []ports.RouteDef{{
			ID: "migration", Session: &ports.RouteSessionDef{
				SessionID: "exclusive-mqtt", LeaseTTL: leaseTTL, StepDownGrace: stepDownGrace,
			},
		}},
	}
}

func TestBuilderPlan_ExclusiveActivationConservativeBoundMayExceedLeaseTTL(t *testing.T) {
	plan, err := NewBuilder(activationTimingBlueprint("45s", "5s", ports.SessionActivationTiming{
		WorstCaseDuration: 4 * time.Minute,
	})).Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan with renewable conservative activation bound: %v", err)
	}
	plan.Close()
}

func TestConfigureSessionActivationTimingExactHardBoundBoundary(t *testing.T) {
	derived := session.HAConfig("exclusive-mqtt", true)
	derivedDef := &ports.SessionDef{SessionMode: string(connectivity.SessionExclusive), Config: activationTimingPluginConfig{
		timing: ports.SessionActivationTiming{WorstCaseDuration: 4 * time.Minute},
	}}
	if err := configureSessionActivationTiming("migration", "exclusive-mqtt", &derived, derivedDef); err != nil {
		t.Fatalf("derive hard activation bound: %v", err)
	}
	if derived.PostAcquireActivationTimeout != 4*time.Minute {
		t.Fatalf("derived hard activation bound = %s, want 4m", derived.PostAcquireActivationTimeout)
	}

	sc := session.HAConfig("exclusive-mqtt", true)
	sc.PostAcquireActivationTimeout = 4 * time.Minute
	def := &ports.SessionDef{SessionMode: string(connectivity.SessionExclusive), Config: activationTimingPluginConfig{
		timing: ports.SessionActivationTiming{WorstCaseDuration: 4 * time.Minute},
	}}
	if err := configureSessionActivationTiming("migration", "exclusive-mqtt", &sc, def); err != nil {
		t.Fatalf("exact hard activation boundary: %v", err)
	}

	def.Config = activationTimingPluginConfig{timing: ports.SessionActivationTiming{WorstCaseDuration: 4*time.Minute + time.Nanosecond}}
	err := configureSessionActivationTiming("migration", "exclusive-mqtt", &sc, def)
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "exceeds configured post-acquire activation timeout") {
		t.Fatalf("one nanosecond over hard activation bound = %v, want ErrInvalidConfig", err)
	}
}

func TestBuilderPlan_ProductionLeaseTTLAtFiveSecondFloorAccepted(t *testing.T) {
	plan, err := NewBuilder(activationTimingBlueprint("5s", "1s", ports.SessionActivationTiming{
		WorstCaseDuration: time.Minute,
	})).Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan at production lease TTL floor: %v", err)
	}
	plan.Close()
}

func TestBuilderPlan_ProductionLeaseTTLBelowFiveSecondFloorRejected(t *testing.T) {
	plan, err := NewBuilder(activationTimingBlueprint("4.999999999s", "1s", ports.SessionActivationTiming{
		WorstCaseDuration: time.Minute,
	})).Plan(t.Context())
	if plan != nil {
		plan.Close()
		t.Fatal("sub-floor production lease TTL returned a build plan")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "effective lease_ttl=4.999999999s is below production minimum=5s") {
		t.Fatalf("sub-floor lease TTL error = %v, want exact production floor diagnostic", err)
	}
}

var _ ports.PluginConfig = activationTimingPluginConfig{}
var _ ports.FreezableConfig = activationTimingPluginConfig{}
var _ ports.PostAcquireActivationTimingConfig = activationTimingPluginConfig{}

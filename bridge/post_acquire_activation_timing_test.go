package bridge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
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

func activationTimingBlueprint(timing ports.SessionActivationTiming) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "activation-timing"},
		Sessions: []ports.SessionDef{{
			ID: "exclusive-mqtt", Transport: "timing.test", SessionMode: string(connectivity.SessionExclusive),
			Config: activationTimingPluginConfig{timing: timing},
		}},
		Routes: []ports.RouteDef{{
			ID: "migration", Session: &ports.RouteSessionDef{
				SessionID: "exclusive-mqtt", LeaseTTL: "45s", StepDownGrace: "5s",
			},
		}},
	}
}

func TestBuilderPlan_ExclusiveActivationTimingAtLeaseSafeBoundary(t *testing.T) {
	plan, err := NewBuilder(activationTimingBlueprint(ports.SessionActivationTiming{
		ConnectTimeout: 40 * time.Second, ReconnectTimeout: 40 * time.Second,
		ReconcileTimeout: 40 * time.Second, ReplayGrace: 40 * time.Second,
	})).Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan at exact activation boundary: %v", err)
	}
	plan.Close()
}

func TestBuilderPlan_ExclusiveActivationTimingOneNanosecondOverRejectedBeforeBuild(t *testing.T) {
	const safe = 40 * time.Second
	tests := []struct {
		name   string
		phase  string
		timing ports.SessionActivationTiming
	}{
		{name: "connect", phase: "connect_timeout", timing: ports.SessionActivationTiming{ConnectTimeout: safe + time.Nanosecond}},
		{name: "reconnect", phase: "reconnect_timeout", timing: ports.SessionActivationTiming{ReconnectTimeout: safe + time.Nanosecond}},
		{name: "reconcile", phase: "reconcile_timeout", timing: ports.SessionActivationTiming{ReconcileTimeout: safe + time.Nanosecond}},
		{name: "replay grace", phase: "replay_grace", timing: ports.SessionActivationTiming{ReplayGrace: safe + time.Nanosecond}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := NewBuilder(activationTimingBlueprint(tt.timing)).Plan(t.Context())
			if plan != nil {
				plan.Close()
				t.Fatal("unsafe activation timing returned a build plan")
			}
			if err == nil {
				t.Fatal("unsafe activation timing must fail before stores/transports are built")
			}
			if !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("activation timing error = %v, want ErrInvalidConfig", err)
			}
			want := "session \"exclusive-mqtt\": configured " + tt.phase + "=40.000000001s exceeds lease-safe post-acquire activation budget=40s (LeaseTTL=45s - teardown_margin=5s)"
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("activation timing error = %v, want substring %q", err, want)
			}
		})
	}
}

var _ ports.PluginConfig = activationTimingPluginConfig{}
var _ ports.FreezableConfig = activationTimingPluginConfig{}
var _ ports.PostAcquireActivationTimingConfig = activationTimingPluginConfig{}

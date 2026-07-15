package bridge

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type failoverTimingPluginConfig struct {
	timing ports.TransportFailoverTiming
}

func (failoverTimingPluginConfig) Kind() string                             { return "timing.failover" }
func (failoverTimingPluginConfig) Validate() error                          { return nil }
func (c failoverTimingPluginConfig) FreezePluginConfig() ports.PluginConfig { return c }
func (c failoverTimingPluginConfig) TransportFailoverTiming(connectivity.SessionMode) ports.TransportFailoverTiming {
	return c.timing
}

type failoverNoTimingPluginConfig struct{}

func (failoverNoTimingPluginConfig) Kind() string                             { return "timing.missing" }
func (failoverNoTimingPluginConfig) Validate() error                          { return nil }
func (c failoverNoTimingPluginConfig) FreezePluginConfig() ports.PluginConfig { return c }

func failoverBudgetBlueprint(slo string, cfg ports.PluginConfig) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "failover-budget", DeploymentMode: "clustered"},
		Sessions: []ports.SessionDef{{ID: "exclusive", Transport: cfg.Kind(), SessionMode: string(connectivity.SessionExclusive), Config: cfg}},
		Routes: []ports.RouteDef{{ID: "route", Session: &ports.RouteSessionDef{
			SessionID: "exclusive", LeaseTTL: "5s", RenewInterval: "1s", RenewCallTimeout: "1s",
			AcquirePollInterval: "4s", StepDownGrace: "1s", MaxRenewFails: 1, FailoverSLO: slo, StartupAllowance: "4s",
		}}},
	}
}

func TestBuilderPlan_FailoverBudgetExactBoundaryPasses(t *testing.T) {
	cfg := failoverBudgetBlueprint("20s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
	plan, err := NewBuilder(cfg).Plan(t.Context())
	if err != nil {
		t.Fatalf("exact boundary: %v", err)
	}
	plan.Close()
}

func TestBuilderPlan_FailoverBudgetOneNanosecondExcessRejects(t *testing.T) {
	cfg := failoverBudgetBlueprint("19.999999999s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
	plan, err := NewBuilder(cfg).Plan(t.Context())
	if plan != nil {
		plan.Close()
		t.Fatal("invalid budget returned plan")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "exceeds declared failover_slo") {
		t.Fatalf("error=%v", err)
	}
}

func TestBuilderPlan_DeclaredFailoverSLORequiresKnownTransportTiming(t *testing.T) {
	cases := map[string]ports.PluginConfig{
		"missing-capability": failoverNoTimingPluginConfig{},
		"unknown-activation": failoverTimingPluginConfig{},
	}
	for name, plugin := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := failoverBudgetBlueprint("30s", plugin)
			plan, err := NewBuilder(cfg).Plan(t.Context())
			if plan != nil {
				plan.Close()
				t.Fatal("unknown timing returned plan")
			}
			if !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestBuilderPlan_UndeclaredFailoverSLODoesNotRequireTimingCapability(t *testing.T) {
	plan, err := NewBuilder(failoverBudgetBlueprint("", failoverNoTimingPluginConfig{})).Plan(t.Context())
	if err != nil {
		t.Fatalf("undeclared SLO: %v", err)
	}
	plan.Close()
}

type failoverCountingStoreFactory struct{ calls atomic.Int32 }

func (f *failoverCountingStoreFactory) NewLeaseStore(context.Context, ports.PluginConfig) (ports.LeaseStore, error) {
	f.calls.Add(1)
	return nil, nil
}
func (f *failoverCountingStoreFactory) NewOutboxStore(context.Context, ports.PluginConfig, ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	f.calls.Add(1)
	return nil, nil
}
func (f *failoverCountingStoreFactory) NewDLQStore(context.Context, ports.PluginConfig) (ports.DLQStore, error) {
	f.calls.Add(1)
	return nil, nil
}

func TestBuilderPlan_FailoverBudgetRejectsBeforeStores(t *testing.T) {
	cfg := failoverBudgetBlueprint("19.999999999s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
	cfg.Stores.Lease = &ports.StoreConfig{Type: "count"}
	factory := &failoverCountingStoreFactory{}
	plan, err := NewBuilder(cfg).RegisterStoreFactory("count", factory).Plan(t.Context())
	if plan != nil {
		plan.Close()
	}
	if err == nil {
		t.Fatal("invalid budget accepted")
	}
	if got := factory.calls.Load(); got != 0 {
		t.Fatalf("store calls=%d", got)
	}
}

func TestCheckedFailoverBudgetArithmeticRejectsOverflow(t *testing.T) {
	_, err := checkedFailoverBudget(time.Duration(math.MaxInt64), time.Nanosecond, time.Nanosecond, time.Nanosecond, time.Nanosecond)
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("overflow=%v", err)
	}
}

var _ ports.PluginConfig = failoverTimingPluginConfig{}
var _ ports.FreezableConfig = failoverTimingPluginConfig{}
var _ ports.TransportFailoverTimingConfig = failoverTimingPluginConfig{}

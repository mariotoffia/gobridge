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
	"github.com/mariotoffia/gobridge/domain/routing"
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
			BrokerHealthStepDown: routing.BrokerPathFailoverOff,
		}}},
	}
}

func TestBuilderPlan_FailoverBudgetExactBoundaryPasses(t *testing.T) {
	cfg := failoverBudgetBlueprint("27s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
	plan, err := NewBuilder(cfg).Plan(t.Context())
	if err != nil {
		t.Fatalf("exact boundary: %v", err)
	}
	plan.Close()
}

func TestBuilderPlan_FailoverBudgetOneNanosecondExcessRejects(t *testing.T) {
	cfg := failoverBudgetBlueprint("26.999999999s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
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
	cfg := failoverBudgetBlueprint("26.999999999s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
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

func TestCheckedFailoverBudgetCountsBothPollBoundariesAndAcquireCallTimeouts(t *testing.T) {
	got, err := checkedFailoverBudget(5*time.Second, 4*time.Second, time.Second, 5*time.Second, 4*time.Second)
	if err != nil {
		t.Fatalf("checked budget: %v", err)
	}
	if got != 27*time.Second {
		t.Fatalf("budget=%s want 27s = TTL 5s + 2*poll 5s + 3 calls*1s + activation 5s + startup 4s", got)
	}
}

func TestCheckedFailoverBudgetCeilsEachNonDivisiblePollBoundary(t *testing.T) {
	got, err := checkedFailoverBudget(time.Nanosecond, 3*time.Nanosecond, 2*time.Nanosecond, 5*time.Nanosecond, 0)
	if err != nil {
		t.Fatalf("checked non-divisible budget: %v", err)
	}
	// ceil(1.25*3ns)=4ns independently for baseline and threshold boundaries.
	if got != 2*time.Millisecond+10*time.Nanosecond {
		t.Fatalf("budget=%s want 2ms+10ns", got)
	}
}

func TestFailoverObservationCallCountUsesMinimumJitteredPoll(t *testing.T) {
	minPoll, maxPoll, err := checkedFailoverPollBounds(5 * time.Second)
	if err != nil {
		t.Fatalf("poll bounds: %v", err)
	}
	if minPoll != 3750*time.Millisecond || maxPoll != 6250*time.Millisecond {
		t.Fatalf("poll bounds min=%s max=%s", minPoll, maxPoll)
	}
	calls, err := checkedObservationCallCount(6*time.Second, minPoll)
	if err != nil {
		t.Fatalf("call count: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d want baseline 1 + ceil(6s/3.75s)=3", calls)
	}
}

func TestCheckedFailoverBudgetFortyFiveSecondProfileCountsThirteenCalls(t *testing.T) {
	minPoll, _, err := checkedFailoverPollBounds(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := checkedObservationCallCount(45*time.Second, minPoll)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 13 {
		t.Fatalf("calls=%d want 13", calls)
	}
	got, err := checkedFailoverBudget(45*time.Second, 5*time.Second, 3*time.Second, 7*time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != 105500*time.Millisecond {
		t.Fatalf("budget=%s want 105.5s", got)
	}
}

func TestFailoverPollBoundsTinyPollClampAndNonDivisibleTTL(t *testing.T) {
	minPoll, maxPoll, err := checkedFailoverPollBounds(3 * time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if minPoll != time.Millisecond || maxPoll != time.Millisecond {
		t.Fatalf("tiny bounds min=%s max=%s", minPoll, maxPoll)
	}
	calls, err := checkedObservationCallCount(1001*time.Microsecond, minPoll)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("non-divisible calls=%d want 3", calls)
	}
}

func TestCheckedObservationCallBudgetOverflowFailsClosed(t *testing.T) {
	_, err := checkedFailoverBudget(time.Duration(math.MaxInt64), time.Nanosecond, 2*time.Nanosecond, time.Nanosecond, 0)
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("call budget overflow=%v", err)
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

func TestBuilderPlan_SharedSessionRejectsDivergentManagerBudgetInputsRegardlessOfRouteOrder(t *testing.T) {
	makeConfig := func(reverse bool) *ports.BridgeConfig {
		cfg := failoverBudgetBlueprint("27s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
		tight := cfg.Routes[0]
		tight.ID = "tight"
		slow := tight
		slow.ID = "slow"
		slowSession := *tight.Session
		slowSession.LeaseTTL = "10s"
		slowSession.FailoverSLO = "32s"
		slow.Session = &slowSession
		if reverse {
			cfg.Routes = []ports.RouteDef{slow, tight}
		} else {
			cfg.Routes = []ports.RouteDef{tight, slow}
		}
		return cfg
	}
	for _, reverse := range []bool{false, true} {
		name := "tight-first"
		if reverse {
			name = "slow-first"
		}
		t.Run(name, func(t *testing.T) {
			plan, err := NewBuilder(makeConfig(reverse)).Plan(t.Context())
			if plan != nil {
				plan.Close()
				t.Fatal("divergent shared session returned plan")
			}
			if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "divergent session-manager configuration") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestBuilderPlan_SharedSessionRejectsDivergentSLOWithSameLeaseTiming(t *testing.T) {
	cfg := failoverBudgetBlueprint("27s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
	second := cfg.Routes[0]
	second.ID = "second"
	secondSession := *second.Session
	secondSession.FailoverSLO = "21s"
	second.Session = &secondSession
	cfg.Routes = append(cfg.Routes, second)
	plan, err := NewBuilder(cfg).Plan(t.Context())
	if plan != nil {
		plan.Close()
		t.Fatal("divergent SLO returned plan")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "divergent session-manager configuration") {
		t.Fatalf("error=%v", err)
	}
}

func TestBuilderPlan_SharedSessionAcceptsIdenticalManagerBudgetInputs(t *testing.T) {
	cfg := failoverBudgetBlueprint("27s", failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}})
	second := cfg.Routes[0]
	second.ID = "second"
	secondSession := *second.Session
	second.Session = &secondSession
	cfg.Routes = append(cfg.Routes, second)
	plan, err := NewBuilder(cfg).Plan(t.Context())
	if err != nil {
		t.Fatalf("identical shared manager inputs: %v", err)
	}
	plan.Close()
}

func TestCheckedFailoverBudgetRejectsBoundaryMultiplicationOverflow(t *testing.T) {
	cases := map[string]struct{ poll, call time.Duration }{
		"two-polls": {poll: time.Duration(math.MaxInt64 / 2), call: time.Nanosecond},
		"two-calls": {poll: time.Nanosecond, call: time.Duration(math.MaxInt64/2 + 1)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := checkedFailoverBudget(time.Nanosecond, tc.poll, tc.call, time.Nanosecond, 0)
			if !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("overflow error=%v", err)
			}
		})
	}
}

package bridge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

func brokerPathBlueprint(slo, brokerHealthStepDown string) *ports.BridgeConfig {
	cfg := failoverBudgetBlueprint(slo, failoverTimingPluginConfig{
		timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second},
	})
	cfg.Routes[0].Session.BrokerHealthStepDown = brokerHealthStepDown
	return cfg
}

// A declared failover_slo is a claim about how long a takeover can take. Left
// undeclared, broker-path failover is off, so the node-local broker outage is
// silently outside that claim — and nothing in the configuration says so.
func TestBuilderPlan_DeclaredFailoverSLORequiresABrokerPathDecision(t *testing.T) {
	plan, err := NewBuilder(brokerPathBlueprint("27s", "")).Plan(t.Context())
	if plan != nil {
		plan.Close()
		t.Fatal("undeclared broker-path policy returned plan")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), routing.BrokerPathFailoverOff) {
		t.Fatalf("error=%v; want ErrInvalidConfig naming the explicit opt-out spelling", err)
	}
}

func TestBuilderPlan_ExplicitlyDisabledBrokerPathFailoverIsAdmitted(t *testing.T) {
	plan, err := NewBuilder(brokerPathBlueprint("27s", routing.BrokerPathFailoverOff)).Plan(t.Context())
	if err != nil {
		t.Fatalf("explicit off: %v", err)
	}
	plan.Close()
}

// The broker-path failure mode has its own budget: threshold detection lands on
// a renew tick, the owner tears down and releases, and only then can a standby
// poll, acquire and activate. 27s of that is fixed by this blueprint, so the
// threshold itself is what decides admission against a 34s objective — which the
// 27s owner-death budget of the same blueprint clears comfortably.
func TestBuilderPlan_BrokerPathStepDownIsBudgetedAgainstTheDeclaredSLO(t *testing.T) {
	plan, err := NewBuilder(brokerPathBlueprint("34s", "7s")).Plan(t.Context())
	if err != nil {
		t.Fatalf("broker-path budget exactly at the objective: %v", err)
	}
	plan.Close()

	plan, err = NewBuilder(brokerPathBlueprint("34s", "8s")).Plan(t.Context())
	if plan != nil {
		plan.Close()
		t.Fatal("broker-path budget over the objective returned plan")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "broker-path") {
		t.Fatalf("error=%v; want ErrInvalidConfig naming the broker-path failure mode", err)
	}
}

// Shared sessions are first-wins at runtime, so two routes that disagree about
// the broker-path policy would silently run one of them.
func TestBuilderPlan_SharedSessionRejectsDivergentBrokerPathPolicy(t *testing.T) {
	cfg := brokerPathBlueprint("34s", "7s")
	second := cfg.Routes[0]
	second.ID = "second"
	secondSession := *second.Session
	secondSession.BrokerHealthStepDown = routing.BrokerPathFailoverOff
	second.Session = &secondSession
	cfg.Routes = append(cfg.Routes, second)

	plan, err := NewBuilder(cfg).Plan(t.Context())
	if plan != nil {
		plan.Close()
		t.Fatal("divergent broker-path policy returned plan")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), "divergent session-manager configuration") {
		t.Fatalf("error=%v", err)
	}
}

func TestCheckedBrokerPathFailoverBudgetSumsDetectionTeardownAndTakeover(t *testing.T) {
	got, err := checkedBrokerPathFailoverBudget(brokerPathBudgetInputs{
		brokerHealthStepDown:   time.Second,
		renewInterval:          time.Second,
		renewJitter:            0,
		renewCallTimeout:       time.Second,
		acquirePoll:            4 * time.Second,
		stepDownGrace:          time.Second,
		postTakeoverActivation: 5 * time.Second,
		startupAllowance:       4 * time.Second,
	})
	if err != nil {
		t.Fatalf("broker-path budget: %v", err)
	}
	// 1 threshold + 3 detection round (interval 1 + two 1s store calls)
	// + 2 teardown/release bounds + 1 grace + 10 two poll boundaries
	// + 2 two acquire calls + 5 activation + 4 startup.
	if got != 28*time.Second {
		t.Fatalf("budget=%s want 28s", got)
	}
}

// The detection round carries the AUTHORITATIVE-READ call as well as the renew
// call: once a renew streak reaches MaxRenewFails the loop re-runs Current on
// every subsequent round, and a node-local fault that takes out the broker path
// can degrade the store path with it.
func TestCheckedBrokerPathFailoverBudgetCountsBothStoreCallsPerDetectionRound(t *testing.T) {
	in := brokerPathBudgetInputs{
		brokerHealthStepDown:   time.Second,
		renewInterval:          time.Second,
		renewCallTimeout:       time.Second,
		acquirePoll:            4 * time.Second,
		stepDownGrace:          time.Second,
		postTakeoverActivation: 5 * time.Second,
	}
	base, err := checkedBrokerPathFailoverBudget(in)
	if err != nil {
		t.Fatal(err)
	}
	in.renewCallTimeout = 2 * time.Second
	doubled, err := checkedBrokerPathFailoverBudget(in)
	if err != nil {
		t.Fatal(err)
	}
	// Two calls in the detection round plus two standby Acquire calls: four in
	// all, so a one-second increase moves the budget by four seconds.
	if got := doubled - base; got != 4*time.Second {
		t.Fatalf("renew_call_timeout +1s moved the budget by %s, want 4s", got)
	}
}

// A standby whose Acquire is already in flight when the Release commits loses
// its observation compare-and-set, reads that as ordinary contention, and waits
// a full poll before the Acquire that wins — the same straddle the owner-death
// formula budgets two boundaries for.
func TestCheckedBrokerPathFailoverBudgetBudgetsBothPollBoundaries(t *testing.T) {
	in := brokerPathBudgetInputs{
		brokerHealthStepDown:   time.Second,
		renewInterval:          time.Second,
		renewCallTimeout:       time.Second,
		acquirePoll:            4 * time.Second,
		stepDownGrace:          time.Second,
		postTakeoverActivation: 5 * time.Second,
	}
	base, err := checkedBrokerPathFailoverBudget(in)
	if err != nil {
		t.Fatal(err)
	}
	in.acquirePoll = 8 * time.Second
	wider, err := checkedBrokerPathFailoverBudget(in)
	if err != nil {
		t.Fatal(err)
	}
	// ceil(1.25 x 8s) - ceil(1.25 x 4s) = 10s - 5s, twice.
	if got := wider - base; got != 10*time.Second {
		t.Fatalf("doubling the acquire poll moved the budget by %s, want 2 x 5s", got)
	}
}

// A zero step_down_grace is not "no grace": the manager substitutes its default
// and really does hold the lease that long before releasing.
func TestBrokerPathBudgetResolvesAZeroStepDownGraceTheWayTheManagerDoes(t *testing.T) {
	cfg := session.DefaultConfig("bench", true)
	cfg.StepDownGrace = 0
	grace, releaseBound := cfg.EffectiveStepDownTiming()
	if grace != session.DefaultConfig("bench", true).StepDownGrace {
		t.Fatalf("grace=%s want the manager's substituted default", grace)
	}
	if releaseBound != session.BoundedReleaseTimeout(grace) {
		t.Fatalf("releaseBound=%s want the bound derived from the resolved grace", releaseBound)
	}
}

func TestCheckedDurationSumRejectsANegativeTerm(t *testing.T) {
	if _, err := checkedDurationSum(time.Second, -time.Second); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("negative term err=%v want ErrInvalidConfig", err)
	}
}

func TestCheckedBrokerPathFailoverBudgetRejectsOverflow(t *testing.T) {
	_, err := checkedBrokerPathFailoverBudget(brokerPathBudgetInputs{
		brokerHealthStepDown:   time.Duration(1) << 62,
		renewInterval:          time.Duration(1) << 62,
		renewCallTimeout:       time.Duration(1) << 62,
		acquirePoll:            time.Duration(1) << 62,
		stepDownGrace:          time.Second,
		postTakeoverActivation: time.Second,
	})
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("overflow=%v want ErrInvalidConfig", err)
	}
}

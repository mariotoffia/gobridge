package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApply_UnchangedUnitKeepsItsSession(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.NoError(t, err)
	assert.Equal(t, InPlaceApplied, outcome)
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "owner a keeps its one session open")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("b-s"), "owner b's session is closed once and replaced")
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
	assert.Equal(t, 7, runtimeRoutePolicy(t, rt, "b").MaxInFlight, "the runtime runs owner b's new route")
	assert.True(t, rt.IsRunning())
}

func TestApply_SerializedClosesOldBeforeBuildingNew(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.True(t, plan.Serialized())

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.NoError(t, err)
	assert.Equal(t, InPlaceApplied, outcome)
	closeOld, newSession := tf.eventIndex("close:b-s#1"), tf.eventIndex("new:b-s#2")
	require.NotEqual(t, -1, closeOld)
	require.NotEqual(t, -1, newSession)
	assert.Less(t, closeOld, newSession, "an exclusive identity is released before its successor is built")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"))
}

func TestApply_BuildFirstWhenNotSerialized(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.False(t, plan.Serialized())

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.NoError(t, err)
	assert.Equal(t, InPlaceApplied, outcome)
	closeOld, newSession := tf.eventIndex("close:b-s#1"), tf.eventIndex("new:b-s#2")
	require.NotEqual(t, -1, closeOld)
	require.NotEqual(t, -1, newSession)
	assert.Less(t, newSession, closeOld, "the successor is built while the old unit still serves")
}

func TestApply_AddedAndRemovedUnits(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, applyTestConfig("a", "c"))

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.NoError(t, err)
	assert.Equal(t, InPlaceApplied, outcome)
	assert.Equal(t, []string{"a", "c"}, runtimeRouteIDs(rt))
	assert.Equal(t, []int{0}, tf.closeCounts("c-s"), "the added owner runs")
	assert.Equal(t, []int{1}, tf.closeCounts("b-s"), "the removed owner is closed")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "the untouched owner keeps its session")
}

// A rule that only the whole document breaks must still refuse the reload:
// in place is never more permissive than a full build.
func TestApply_PreflightFailureChangesNothing(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf, WithBlueprintValidator(func(cfg *ports.BridgeConfig) error {
		if len(cfg.Routes) > 2 {
			return errTooManyRoutes
		}
		return nil
	}))
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, applyTestConfig("a", "b", "c"))

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, errTooManyRoutes)
	assert.Equal(t, InPlaceUnchanged, outcome)
	assert.Empty(t, tf.closeCounts("c-s"), "no part is built")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"))
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"))
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
}

// A part the store checks refuse is refused before any unit retires, even in a
// serialized reload: a configuration a full build refuses before it stops the
// old runtime never costs a running unit its session here either.
func TestApply_StoreCheckRefusalRetiresNothing(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	newBuilder := func(cfg *ports.BridgeConfig) *Builder {
		return NewBuilder(cfg).
			RegisterTransportFactory("tracked", tf).
			RegisterStoreFactory("lease-store", &durabilityStoreFactory{crashDurable: false}).
			RegisterStoreFactory("outbox-store", &durabilityStoreFactory{crashDurable: true})
	}
	withStores := func(cfg *ports.BridgeConfig) *ports.BridgeConfig {
		cfg.Stores = ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "lease-store"},
			Outbox: &ports.StoreConfig{Type: "outbox-store"},
		}
		return cfg
	}
	running := withStores(applyTestConfig("a", "b"))
	rt := startApplyTestRuntime(t, newBuilder, running)
	next := withStores(applyTestConfig("a", "b"))
	next.Routes[1].DeliveryMode = "shared_outbox"
	plan := planApplyTest(t, tf, running, next)
	require.True(t, plan.Serialized())

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorContains(t, err, "crash-durable OutboxStore")
	assert.Equal(t, InPlaceUnchanged, outcome)
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "owner b is not retired")
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
}

func TestApply_BuildFailureBeforeRetireChangesNothing(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	oldPolicy := runtimeRoutePolicy(t, rt, "b")
	// Owner c is added before owner b's successor, so one part is built before
	// the failing one.
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "c", "b"), "b"))
	require.False(t, plan.Serialized())
	tf.refuseSessions("b-s", 1)

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, errSessionRefused)
	assert.Equal(t, InPlaceUnchanged, outcome)
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "the old owner b is untouched")
	assert.Equal(t, []int{1}, tf.closeCounts("c-s"), "the part built before the failure is released")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"))
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
	assert.Equal(t, oldPolicy, runtimeRoutePolicy(t, rt, "b"), "owner b still runs its old route")
}

func TestApply_BuildFailureAfterRetireRestoresOldUnit(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	oldPolicy := runtimeRoutePolicy(t, rt, "b")
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.True(t, plan.Serialized())
	tf.refuseSessions("b-s", 1)

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, errSessionRefused)
	assert.Equal(t, InPlaceUnchanged, outcome)
	assert.Equal(t, []int{1, 0}, tf.closeCounts("b-s"), "the retired unit is rebuilt on a new session")
	assert.Equal(t, oldPolicy, runtimeRoutePolicy(t, rt, "b"), "the rebuilt unit runs the old route")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "owner a is untouched")
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
	assert.True(t, rt.IsRunning())
}

func TestApply_RestoreFailureIsTorn(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.True(t, plan.Serialized())
	tf.refuseSessions("b-s", -1)

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, errSessionRefused)
	assert.Equal(t, InPlaceTorn, outcome)
	assert.Equal(t, []int{1}, tf.closeCounts("b-s"))
	assert.Equal(t, []string{"a"}, runtimeRouteIDs(rt), "the runtime runs neither configuration")
}

func TestApply_RetireFailureIsWedged(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.False(t, plan.Serialized())
	tf.refuseClose("b-s", 1, errCloseRefused)

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, errCloseRefused)
	assert.Equal(t, InPlaceWedged, outcome)
	assert.Equal(t, []int{1, 1}, tf.closeCounts("b-s"), "the successor built before the retire is released, not grafted")
	assert.Equal(t, []string{"a"}, runtimeRouteIDs(rt), "nothing is grafted after a failed retire")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"))
}

func TestApply_NotRunningIsUnchanged(t *testing.T) {
	cases := map[string]func(t *testing.T, rt *runtime.Runtime){
		"never started": func(*testing.T, *runtime.Runtime) {},
		"stopped": func(t *testing.T, rt *runtime.Runtime) {
			require.NoError(t, rt.Start(context.Background()))
			require.NoError(t, rt.Stop(context.Background()))
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			tf := newPerSessionTransportFactory(false)
			newBuilder := applyTestBuilder(tf)
			running := applyTestConfig("a", "b")
			rt, err := newBuilder(running).Build(context.Background())
			require.NoError(t, err)
			t.Cleanup(func() { _ = rt.Stop(context.Background()) })
			prepare(t, rt)
			plan := planApplyTest(t, tf, running, applyTestConfig("a", "b", "c"))

			outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

			require.ErrorContains(t, err, "not running")
			assert.Equal(t, InPlaceUnchanged, outcome)
			assert.Empty(t, tf.closeCounts("c-s"), "no part is built")
		})
	}
}

// A runtime that stops running after Apply checked it refuses the first
// retire. Nothing was taken out, so no ownership is in doubt and the reload
// changes nothing rather than wedging.
func TestApply_FirstRetireRefusedByAStoppedRuntimeIsUnchanged(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	var fence *runtime.Runtime
	newBuilder := applyTestBuilder(tf, WithBlueprintValidator(func(*ports.BridgeConfig) error {
		if fence != nil {
			fence.Fence() // the runtime stops running while the next document is preflighted
		}
		return nil
	}))
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	fence = rt
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.False(t, plan.Serialized())

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, runtime.ErrNotRunning)
	assert.Equal(t, InPlaceUnchanged, outcome)
	assert.Equal(t, []int{0, 1}, tf.closeCounts("b-s"),
		"the running unit is not retired, and the successor built for it is released")
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
}

// A runtime that stops running between two retires refuses the second. The
// first unit is gone, but nothing was taken out for the second, so no
// ownership is in doubt: the runtime runs neither configuration, and its caller
// rebuilds the running one rather than restarting the process.
func TestApply_LaterRetireRefusedByAStoppedRuntimeIsTorn(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b", "c")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(changeRoute(applyTestConfig("a", "b", "c"), "b"), "c"))
	require.False(t, plan.Serialized())
	require.Len(t, plan.retire, 2)
	first, second := plan.retire[0].sessions[0], plan.retire[1].sessions[0]
	var once sync.Once
	tf.onClose = func(name string) {
		if name == first+"#1" {
			once.Do(rt.Fence) // the runtime stops running while the first unit retires
		}
	}

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, runtime.ErrNotRunning)
	assert.Equal(t, InPlaceTorn, outcome)
	assert.Equal(t, []int{1, 1}, tf.closeCounts(first), "the first unit retired, and its successor is released")
	assert.Equal(t, []int{0, 1}, tf.closeCounts(second), "the second unit is not retired, and its successor is released")
}

// One added unit listed twice, a plan no split produces, makes the second
// graft collide with the first. The unit already grafted is retired again and
// the running unit restored.
func TestApply_GraftFailureRetiresGraftedUnitsAndRestores(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	oldPolicy := runtimeRoutePolicy(t, rt, "b")
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.False(t, plan.Serialized())
	plan.add = append(plan.add, plan.add[0])

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorContains(t, err, "already registered")
	assert.Equal(t, InPlaceUnchanged, outcome)
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
	assert.Equal(t, oldPolicy, runtimeRoutePolicy(t, rt, "b"), "the running unit is restored")
	// #1 the running unit, retired; #2 the grafted successor, retired again;
	// #3 the refused part, stopped; #4 the restored unit, running.
	assert.Equal(t, []int{1, 1, 1, 0}, tf.closeCounts("b-s"))
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"))
}

func TestApply_GraftedUnitThatDoesNotRetireIsWedged(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.False(t, plan.Serialized())
	plan.add = append(plan.add, plan.add[0])
	tf.refuseClose("b-s", 2, errCloseRefused) // the successor grafted first

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, errCloseRefused)
	assert.Equal(t, InPlaceWedged, outcome)
	assert.Equal(t, []int{1, 1, 1}, tf.closeCounts("b-s"), "nothing is restored after a grafted unit failed to retire")
	assert.Equal(t, []string{"a"}, runtimeRouteIDs(rt))
}

// A retire, and a stop of a part never grafted, run to the end under a context
// the caller has already cancelled: they are detached from it and bounded by
// the drain timeout instead.
func TestInPlaceReload_TeardownIsDetachedFromTheCallersContext(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	part, err := newBuilder(plan.add[0].sub).buildPart(context.Background(), rt)
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, plan.retireUnit(cancelled, rt, plan.retire[0]), "the retired unit's components finish stopping")
	require.NoError(t, plan.stopParts(cancelled, []*runtime.Runtime{part}), "the part's components finish stopping")

	assert.Equal(t, []int{1, 1}, tf.closeCounts("b-s"), "the retired session and the part's session are closed")
	assert.Equal(t, []string{"a"}, runtimeRouteIDs(rt))
}

// A retire and a part stop are bounded by the drain timeout the caller gives,
// and by the running configuration's drain timeout (1s here) when it gives
// none. The budget is read as a value, not off the wall clock, so a scheduler
// pause cannot blur it.
func TestInPlaceReload_TeardownIsBoundedByTheCallersDrainTimeout(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	running := applyTestConfig("a", "b")
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))

	ctx, cancel := plan.teardownCtx(context.Background())
	defer cancel()
	_, ok := ctx.Deadline()
	assert.True(t, ok, "a teardown is always bounded")
	assert.Equal(t, time.Second, plan.teardownBudget(), "without one, the running configuration's drain timeout")
	plan.DrainTimeout = time.Hour
	assert.Equal(t, time.Hour, plan.teardownBudget(), "the caller's drain timeout")
}

// A serialized reload retires its units between preparing the parts and
// committing them, and a retire may take the whole drain timeout. The commit
// therefore runs in a phase of its own, begun once the retire is over: a budget
// that ran out while the retired unit drained must not fail the build that
// follows.
func TestApply_CommitGetsAFreshPhaseAfterASlowRetire(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.True(t, plan.Serialized())

	var mu sync.Mutex
	var phases []context.Context
	var cancels []context.CancelFunc
	phasesAtRetire := -1
	phase := func(ctx context.Context) (context.Context, context.CancelFunc) {
		phaseCtx, cancel := context.WithCancel(ctx)
		mu.Lock()
		defer mu.Unlock()
		phases = append(phases, phaseCtx)
		cancels = append(cancels, cancel)
		return phaseCtx, cancel
	}
	// The retired session is slow to close: by the time it has, the budget of
	// every phase begun so far is spent.
	tf.onClose = func(name string) {
		if name != "b-s#1" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		phasesAtRetire = len(phases)
		for _, cancel := range cancels {
			cancel()
		}
	}

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, phase)

	require.NoError(t, err, "the commit must not run under a budget the retire spent")
	assert.Equal(t, InPlaceApplied, outcome)
	assert.Equal(t, 1, phasesAtRetire, "only preparing the parts has begun when the retired unit closes")
	require.Len(t, phases, 2, "preparing and committing the parts are two phases")
	assert.Error(t, phases[1].Err(), "a phase is released when it ends")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("b-s"), "owner b's session is replaced")
	assert.Equal(t, 7, runtimeRoutePolicy(t, rt, "b").MaxInFlight)
}

func TestInPlaceOutcome_String(t *testing.T) {
	for outcome, want := range map[InPlaceOutcome]string{
		InPlaceApplied:     "applied",
		InPlaceUnchanged:   "unchanged",
		InPlaceTorn:        "torn",
		InPlaceWedged:      "wedged",
		InPlaceOutcome(99): "InPlaceOutcome(99)",
	} {
		assert.Equal(t, want, outcome.String())
	}
}

package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errSessionRefused = errors.New("session refused")
	errCloseRefused   = errors.New("close refused")
	errTooManyRoutes  = errors.New("too many routes")
)

// perSessionTransportFactory builds a new recordedSession for every session it
// is asked for, and logs in order each session it builds and each close:
// "new:<id>#<n>" and "close:<id>#<n>", n counting the sessions built for id.
// It advertises plan-driven subscriptions, so the builder gives every session
// a receiver rides on a manager, which closes it when its unit retires or its
// part stops.
type perSessionTransportFactory struct {
	fakeTransportFactory
	caps []ports.Capability

	mu       sync.Mutex
	events   []string
	sessions map[string][]*recordedSession
	failures map[string]int // session id → NewSession calls left to refuse; negative refuses every call
}

func newPerSessionTransportFactory(exclusive bool) *perSessionTransportFactory {
	caps := []ports.Capability{ports.CapStatefulSession, ports.CapPlanDrivenSubscriptions, ports.CapSourceRedelivery}
	if exclusive {
		caps = append(caps, ports.CapExclusiveIdentity)
	}
	return &perSessionTransportFactory{
		caps:     caps,
		sessions: make(map[string][]*recordedSession),
		failures: make(map[string]int),
	}
}

func (f *perSessionTransportFactory) Capabilities() []ports.Capability { return f.caps }

func (f *perSessionTransportFactory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if left := f.failures[spec.ID]; left != 0 {
		if left > 0 {
			f.failures[spec.ID] = left - 1
		}
		return nil, errSessionRefused
	}
	s := &recordedSession{factory: f, name: fmt.Sprintf("%s#%d", spec.ID, len(f.sessions[spec.ID])+1)}
	f.sessions[spec.ID] = append(f.sessions[spec.ID], s)
	f.events = append(f.events, "new:"+s.name)
	return s, nil
}

// refuseSessions makes the next times NewSession calls for id fail; a negative
// times fails every one.
func (f *perSessionTransportFactory) refuseSessions(id string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[id] = times
}

// refuseClose makes the n-th session built for id fail its Close with err.
func (f *perSessionTransportFactory) refuseClose(id string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id][n-1].closeErr = err
}

// closeCounts lists, oldest first, how often each session built for id was
// closed.
func (f *perSessionTransportFactory) closeCounts(id string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var counts []int
	for _, s := range f.sessions[id] {
		counts = append(counts, s.closes)
	}
	return counts
}

// eventIndex returns where event is in the log, or -1.
func (f *perSessionTransportFactory) eventIndex(event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Index(f.events, event)
}

// recordedSession is one session a perSessionTransportFactory built. Its
// fields are guarded by the factory's mutex.
type recordedSession struct {
	fakeSession
	factory  *perSessionTransportFactory
	name     string
	closes   int
	closeErr error
}

func (s *recordedSession) Close(context.Context) error {
	s.factory.mu.Lock()
	defer s.factory.mu.Unlock()
	s.closes++
	s.factory.events = append(s.factory.events, "close:"+s.name)
	return s.closeErr
}

var (
	_ ports.TransportFactory = (*perSessionTransportFactory)(nil)
	_ ports.Session          = (*recordedSession)(nil)
)

// applyTestConfig is reloadTestConfig with every owner on the tracked
// transport and every route dropping what it cannot deliver, so a runtime
// builds from it without a DLQ store.
func applyTestConfig(owners ...string) *ports.BridgeConfig {
	cfg := reloadTestConfig()
	for _, owner := range owners {
		addReloadTestOwner(cfg, owner, "tracked")
		cfg.Routes[len(cfg.Routes)-1].Policy = ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"}
	}
	return cfg
}

// changeRoute changes owner's route in cfg: it admits 7 deliveries in flight.
func changeRoute(cfg *ports.BridgeConfig, owner string) *ports.BridgeConfig {
	for i := range cfg.Routes {
		if cfg.Routes[i].ID == owner {
			cfg.Routes[i].Policy.MaxInFlight = 7
		}
	}
	return cfg
}

// applyTestBuilder returns the newBuilder Apply takes: a builder with opts over
// tf as the tracked transport and the memory store.
func applyTestBuilder(tf ports.TransportFactory, opts ...BuilderOption) func(*ports.BridgeConfig) *Builder {
	return func(cfg *ports.BridgeConfig) *Builder {
		return NewBuilder(cfg, opts...).
			RegisterTransportFactory("tracked", tf).
			RegisterStoreFactory("memory", &fakeStoreFactory{})
	}
}

// startApplyTestRuntime builds and starts the runtime a reload is applied to.
// It is stopped when the test ends.
func startApplyTestRuntime(t *testing.T, newBuilder func(*ports.BridgeConfig) *Builder, cfg *ports.BridgeConfig) *runtime.Runtime {
	t.Helper()
	rt, err := newBuilder(cfg).Build(context.Background())
	require.NoError(t, err)
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return rt
}

func planApplyTest(t *testing.T, tf ports.TransportFactory, running, next *ports.BridgeConfig) *InPlaceReload {
	t.Helper()
	plan, ok := PlanInPlaceReload(running, next, map[string]ports.TransportFactory{"tracked": tf})
	require.True(t, ok, "expected the reload to be eligible in place")
	return plan
}

func runtimeRouteIDs(rt *runtime.Runtime) []string {
	var ids []string
	for _, route := range rt.Routes() {
		ids = append(ids, route.ID)
	}
	slices.Sort(ids)
	return ids
}

func runtimeRoutePolicy(t *testing.T, rt *runtime.Runtime, id string) routing.RoutePolicy {
	t.Helper()
	for _, route := range rt.Routes() {
		if route.ID == id {
			return route.Policy
		}
	}
	t.Fatalf("runtime has no route %q", id)
	return routing.RoutePolicy{}
}

func TestApply_UnchangedUnitKeepsItsSession(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

	outcome, err := plan.Apply(context.Background(), rt, newBuilder)

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

			outcome, err := plan.Apply(context.Background(), rt, newBuilder)

			require.ErrorContains(t, err, "not running")
			assert.Equal(t, InPlaceUnchanged, outcome)
			assert.Empty(t, tf.closeCounts("c-s"), "no part is built")
		})
	}
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

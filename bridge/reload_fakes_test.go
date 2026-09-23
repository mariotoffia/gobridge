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
// part stops. Like a transport dialling a broker, it refuses to build a session
// under a context that has ended.
type perSessionTransportFactory struct {
	fakeTransportFactory
	caps []ports.Capability
	// onClose, when set, runs at the start of every session's Close with that
	// session's "<id>#<n>" name. It is read without the lock, so set it before
	// the closes it must see can run.
	onClose func(name string)

	mu        sync.Mutex
	events    []string
	sessions  map[string][]*recordedSession
	failures  map[string]int   // session id → NewSession calls left to refuse; negative refuses every call
	closeErrs map[string]error // "<id>#<n>" → the error that session's Close returns
}

func newPerSessionTransportFactory(exclusive bool) *perSessionTransportFactory {
	caps := []ports.Capability{ports.CapStatefulSession, ports.CapPlanDrivenSubscriptions, ports.CapSourceRedelivery}
	if exclusive {
		caps = append(caps, ports.CapExclusiveIdentity)
	}
	return &perSessionTransportFactory{
		caps:      caps,
		sessions:  make(map[string][]*recordedSession),
		failures:  make(map[string]int),
		closeErrs: make(map[string]error),
	}
}

func (f *perSessionTransportFactory) Capabilities() []ports.Capability { return f.caps }

func (f *perSessionTransportFactory) NewSession(ctx context.Context, spec ports.SessionSpec) (ports.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

// refuseClose makes the n-th session built for id, built yet or not, fail its
// Close with err.
func (f *perSessionTransportFactory) refuseClose(id string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeErrs[fmt.Sprintf("%s#%d", id, n)] = err
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
	factory *perSessionTransportFactory
	name    string
	closes  int
}

func (s *recordedSession) Close(context.Context) error {
	if s.factory.onClose != nil {
		s.factory.onClose(s.name)
	}
	s.factory.mu.Lock()
	defer s.factory.mu.Unlock()
	s.closes++
	s.factory.events = append(s.factory.events, "close:"+s.name)
	return s.factory.closeErrs[s.name]
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

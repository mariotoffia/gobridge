package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	errSessionRefused = errors.New("session refused")
	errCloseRefused   = errors.New("close refused")
)

// trackedTransportFactory builds a new session for every NewSession call and
// counts, per session id, how often each session it built was closed. It
// advertises plan-driven subscriptions, so the builder gives every session a
// receiver rides on a manager, which closes it when its unit retires or its
// runtime stops.
type trackedTransportFactory struct {
	caps []ports.Capability
	// onNewSession, when set, runs first in every NewSession call. It is read
	// without the lock, so set it before the calls it must see.
	onNewSession func(id string)

	mu        sync.Mutex
	closes    map[string][]int // session id → close count of each session built for it, oldest first
	refusals  map[string]int   // session id → NewSession calls left to refuse
	closeErrs map[string]error // "<id>#<n>" → what the n-th session built for id returns from Close
}

func newTrackedTransportFactory(exclusive bool) *trackedTransportFactory {
	caps := []ports.Capability{ports.CapStatefulSession, ports.CapPlanDrivenSubscriptions, ports.CapSourceRedelivery}
	if exclusive {
		caps = append(caps, ports.CapExclusiveIdentity)
	}
	return &trackedTransportFactory{caps: caps, closes: map[string][]int{}, refusals: map[string]int{}, closeErrs: map[string]error{}}
}

func (f *trackedTransportFactory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	if f.onNewSession != nil {
		f.onNewSession(spec.ID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refusals[spec.ID] > 0 {
		f.refusals[spec.ID]--
		return nil, errSessionRefused
	}
	f.closes[spec.ID] = append(f.closes[spec.ID], 0)
	return &trackedSession{factory: f, id: spec.ID, n: len(f.closes[spec.ID])}, nil
}

func (f *trackedTransportFactory) NewReceiver(context.Context, ports.ReceiverSpec, ports.Session) (ports.Receiver, error) {
	return idleReceiver{}, nil
}

func (f *trackedTransportFactory) NewSender(context.Context, ports.SenderSpec, ports.Session) (ports.Sender, error) {
	return discardSender{}, nil
}

func (f *trackedTransportFactory) Capabilities() []ports.Capability         { return f.caps }
func (f *trackedTransportFactory) AddressValidator() ports.AddressValidator { return nil }

// refuseSessions makes the next times NewSession calls for id fail.
func (f *trackedTransportFactory) refuseSessions(id string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals[id] = times
}

// refuseClose makes the n-th session built for id fail its Close with err.
func (f *trackedTransportFactory) refuseClose(id string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeErrs[fmt.Sprintf("%s#%d", id, n)] = err
}

func (f *trackedTransportFactory) closeCounts(id string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.closes[id]...)
}

type trackedSession struct {
	factory *trackedTransportFactory
	id      string
	n       int
}

func (s *trackedSession) Start(context.Context) error                               { return nil }
func (s *trackedSession) Reconcile(context.Context, connectivity.SessionPlan) error { return nil }
func (s *trackedSession) Health(context.Context) ports.SessionHealth                { return ports.SessionHealth{} }
func (s *trackedSession) Events() <-chan ports.SessionEvent                         { return nil }

func (s *trackedSession) Close(context.Context) error {
	s.factory.mu.Lock()
	defer s.factory.mu.Unlock()
	s.factory.closes[s.id][s.n-1]++
	return s.factory.closeErrs[fmt.Sprintf("%s#%d", s.id, s.n)]
}

type idleReceiver struct{}

func (idleReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

type discardSender struct{}

func (discardSender) Send(context.Context, ports.OutboundMessage) error { return nil }

var (
	_ ports.TransportFactory = (*trackedTransportFactory)(nil)
	_ ports.Session          = (*trackedSession)(nil)
	_ ports.Receiver         = idleReceiver{}
	_ ports.Sender           = discardSender{}
)

// inPlaceTestConfig is one pipe per owner on the tracked transport — a
// session, a receiver and a sender on it, a binding and a route named after
// the owner — next to an HTTP unit whose endpoints live on the registry's mux.
func inPlaceTestConfig(owners ...string) *ports.BridgeConfig {
	drop := ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"}
	cfg := &ports.BridgeConfig{
		Version:   1,
		Bridge:    ports.BridgeSettings{ID: "bridge-x", DeploymentMode: "standalone", DrainTimeout: "1s"},
		Receivers: []ports.ReceiverDef{{ID: "h-rx", Transport: "http", Config: httptransport.Config{Path: "/ingress"}}},
		Senders:   []ports.SenderDef{{ID: "h-sse", Transport: "http", Config: httptransport.Config{Mode: "sse", Path: "/events"}}},
		Bindings:  []ports.BindingDef{{ID: "h-b", SenderID: "h-sse", Address: "h-sse"}},
		Routes:    []ports.RouteDef{{ID: "h", ReceiverID: "h-rx", Bindings: []string{"h-b"}, Policy: drop}},
	}
	for _, owner := range owners {
		cfg.Sessions = append(cfg.Sessions, ports.SessionDef{ID: owner + "-s", Transport: "tracked"})
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{ID: owner + "-rx", SessionID: owner + "-s"})
		cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: owner + "-tx", SessionID: owner + "-s"})
		cfg.Bindings = append(cfg.Bindings, ports.BindingDef{ID: owner + "-b", SenderID: owner + "-tx", Address: "topic/" + owner})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID: owner, ReceiverID: owner + "-rx", DeliveryMode: "direct_hold", Bindings: []string{owner + "-b"}, Policy: drop,
		})
	}
	return cfg
}

// withRouteChange returns cfg as version with route id admitting 7
// deliveries in flight.
func withRouteChange(cfg *ports.BridgeConfig, id string, version int) *ports.BridgeConfig {
	cfg.Version = version
	for i := range cfg.Routes {
		if cfg.Routes[i].ID == id {
			cfg.Routes[i].Policy.MaxInFlight = 7
		}
	}
	return cfg
}

// trackedConfig is the plugin config of the tracked transport: it has none.
type trackedConfig struct{}

func (trackedConfig) Kind() string    { return "tracked" }
func (trackedConfig) Validate() error { return nil }

// newInPlaceTestApp returns an App that decodes and builds the tracked
// transport on every path. The runtime it runs when the test ends is stopped.
func newInPlaceTestApp(t testing.TB, tf ports.TransportFactory, resolver parameterResolver) *App {
	t.Helper()
	plugins := newDefaultPluginRegistry()
	require.NoError(t, plugins.Register("tracked", func(ports.RawConfig) (ports.PluginConfig, error) { return trackedConfig{}, nil }))
	app := NewApp(testBootstrapCfg(), WithParameterResolver(resolver), WithPluginRegistry(plugins))
	app.extraTransports = map[string]ports.TransportFactory{"tracked": tf}
	t.Cleanup(func() { _ = stopRuntime(context.Background(), app.CurrentRuntime(), app.CurrentAppliedConfig()) })
	return app
}

// applyTo applies cfg as the watcher and the admin commit do: under the App's
// apply lock.
func applyTo(t testing.TB, app *App, cfg *ports.BridgeConfig) error {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.applyLogicalConfig(t.Context(), cfg, false)
}

func adminKeyResolver() staticParameterResolver {
	return staticParameterResolver{"/admin": "admin-secret-key-123456"}
}

func routeMaxInFlight(t *testing.T, routes []ports.RouteInfo, id string) int {
	t.Helper()
	for _, route := range routes {
		if route.ID == id {
			return route.Policy.MaxInFlight
		}
	}
	t.Fatalf("no route %q", id)
	return 0
}

func configRouteMaxInFlight(t *testing.T, cfg *ports.BridgeConfig, id string) int {
	t.Helper()
	for i := range cfg.Routes {
		if cfg.Routes[i].ID == id {
			return cfg.Routes[i].Policy.MaxInFlight
		}
	}
	t.Fatalf("no route %q", id)
	return 0
}

// sseSenderShutDown reports whether the SSE sender mux serves at /events has
// been drained: a drained sender refuses a subscriber at once, while a live
// one takes it until the request ends — here at once, its context is over.
func sseSenderShutDown(t *testing.T, mux http.Handler) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil))
	return rec.Code == http.StatusServiceUnavailable
}

func TestApplyInPlace_KeepsInstalledRuntimeAndOtherOwnersSessions(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	keys := adminKeyResolver()
	app := newInPlaceTestApp(t, tf, keys)
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt, installed, mux := app.CurrentRuntime(), app.registryRef.Load(), app.handlerRef.Get()
	keys["/admin"] = "rotated-admin-key-654321"

	next := withRouteChange(inPlaceTestConfig("a", "b"), "b", 2)
	require.NoError(t, applyTo(t, app, next))

	assert.Same(t, rt, app.CurrentRuntime(), "the installed runtime is kept")
	assert.True(t, rt.IsRunning())
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "owner a's session is never closed")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("b-s"), "owner b's session is replaced")
	assert.Equal(t, 7, routeMaxInFlight(t, rt.Routes(), "b"), "the runtime runs owner b's new route")
	assert.Same(t, next, app.CurrentAppliedConfig())
	reg := app.registryRef.Load()
	assert.NotSame(t, next, reg.cfg, "the registry holds the resolved config, not the logical one")
	assert.Equal(t, 7, configRouteMaxInFlight(t, reg.cfg, "b"), "the registry holds the config the runtime runs")
	assert.Same(t, installed.http, reg.http, "the HTTP factory the runtime mounted on stays installed")
	assert.Same(t, mux, app.handlerRef.Get(), "the transport server keeps the mux it serves")
	assert.False(t, sseSenderShutDown(t, mux), "the unchanged SSE sender keeps serving")
	assert.Equal(t, "rotated-admin-key-654321", app.apiKeysRef.AdminKey(), "the reload's resolved admin key is installed")
}

// The runtime a reload kept runs a configuration its convergence watch has not
// judged yet, so the reload starts a fresh one, as a swap does for the runtime
// it installs.
func TestApplyInPlace_RestartsTheConvergenceWatch(t *testing.T) {
	app := newInPlaceTestApp(t, newTrackedTransportFactory(false), adminKeyResolver())
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { cancel(); app.watchWg.Wait() })
	app.rootCtx = ctx
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	installs := 0
	app.onRuntimeInstalled = func() { installs++ }
	require.True(t, app.markConvergenceDegraded(app.CurrentRuntime(), "the running configuration has not converged"))

	require.NoError(t, applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2)))

	degraded, _ := app.convergenceDegradedState()
	assert.False(t, degraded, "the reloaded configuration gets a convergence attempt of its own")
	assert.Equal(t, 1, installs)
}

// A withdrawal that lands while the reload builds its parts must not be
// recorded as an applied configuration.
func TestApplyInPlace_WithdrawalDuringApplyIsNotRecordedApplied(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	applied := app.CurrentAppliedConfig()
	tf.onNewSession = func(string) { app.observationEpoch.Add(1) }

	err := applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2))

	require.ErrorContains(t, err, "authorization withdrawn")
	assert.Same(t, applied, app.CurrentAppliedConfig())
}

// A part that cannot be built while the retired unit still serves changes
// nothing: the runtime keeps running the running configuration.
func TestApplyInPlace_FailedBuildKeepsRunningConfig(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt, applied := app.CurrentRuntime(), app.CurrentAppliedConfig()
	tf.refuseSessions("b-s", 1)

	err := applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2))

	require.ErrorIs(t, err, errSessionRefused)
	assert.Same(t, rt, app.CurrentRuntime())
	assert.True(t, rt.IsRunning())
	assert.Same(t, applied, app.CurrentAppliedConfig())
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "owner b is untouched")
	assert.Equal(t, 0, routeMaxInFlight(t, rt.Routes(), "b"))
	assert.False(t, app.runtimeTerminal())
}

func TestApplyInPlace_HTTPUnitChangeUsesFullReplacement(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a")))
	rt := app.CurrentRuntime()

	next := withRouteChange(inPlaceTestConfig("a"), "h", 2)
	require.NoError(t, applyTo(t, app, next))

	assert.NotSame(t, rt, app.CurrentRuntime(), "a change to a unit on the HTTP transport replaces the runtime")
	assert.False(t, rt.IsRunning())
	assert.Same(t, next, app.CurrentAppliedConfig())
	assert.Equal(t, 7, routeMaxInFlight(t, app.CurrentRuntime().Routes(), "h"))
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "a full replacement rebuilds every session")
}

// A serialized reload retires owner b, then neither its successor nor its
// restore can be built: the runtime runs neither configuration, and the App
// replaces it with one built from the running configuration.
func TestApplyInPlace_TornRecoversPrevious(t *testing.T) {
	tf := newTrackedTransportFactory(true)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt, applied, mux := app.CurrentRuntime(), app.CurrentAppliedConfig(), app.handlerRef.Get()
	tf.refuseSessions("b-s", 2)

	err := applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2))

	require.ErrorIs(t, err, errSessionRefused)
	recovered := app.CurrentRuntime()
	require.NotNil(t, recovered)
	assert.NotSame(t, rt, recovered, "the torn runtime is replaced")
	assert.False(t, rt.IsRunning(), "the torn runtime is stopped")
	assert.True(t, recovered.IsRunning())
	assert.False(t, app.runtimeTerminal(), "a successful recovery does not wedge")
	assert.Same(t, applied, app.CurrentAppliedConfig())
	assert.Equal(t, 0, routeMaxInFlight(t, recovered.Routes(), "b"), "the recovered runtime runs the running configuration")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "owner a is rebuilt with the recovered runtime")
	assert.True(t, sseSenderShutDown(t, mux), "the superseded mux's SSE senders are drained")
}

// A torn runtime whose stop fails may still hold the identities a rebuild
// would claim, so the App wedges instead of recovering, as it does when an old
// runtime fails to stop during a prepare/commit swap.
func TestApplyInPlace_TornRuntimeThatDoesNotStopWedges(t *testing.T) {
	tf := newTrackedTransportFactory(true)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	tf.refuseSessions("b-s", 2)
	tf.refuseClose("a-s", 1, errCloseRefused)

	err := applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2))

	require.ErrorIs(t, err, errSessionRefused)
	require.ErrorIs(t, err, errCloseRefused, "the error names why the runtime was not rebuilt")
	assert.True(t, app.runtimeTerminal())
	assert.Nil(t, app.CurrentRuntime())
	assert.Nil(t, app.CurrentAppliedConfig())
	assert.Len(t, tf.closeCounts("a-s"), 1, "nothing is rebuilt over a session that did not close")
}

func TestApplyInPlace_WedgedEntersWedgedState(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	app := newInPlaceTestApp(t, tf, adminKeyResolver())
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt, mux := app.CurrentRuntime(), app.handlerRef.Get()
	tf.refuseClose("b-s", 1, errCloseRefused)

	err := applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2))

	require.ErrorIs(t, err, errCloseRefused)
	assert.True(t, app.runtimeTerminal(), "a unit that did not stop cleanly wedges the App")
	assert.Nil(t, app.CurrentRuntime())
	assert.Nil(t, app.CurrentAppliedConfig())
	assert.False(t, rt.IsRunning(), "the runtime is stopped")
	assert.True(t, sseSenderShutDown(t, mux), "the superseded mux's SSE senders are drained")
}

func TestApplyInPlace_ResolvesInputsOnce(t *testing.T) {
	var resolutions atomic.Int32
	resolver := parameterResolverFunc(func(context.Context, string) (string, error) {
		resolutions.Add(1)
		return "admin-secret-key-123456", nil
	})
	app := newInPlaceTestApp(t, newTrackedTransportFactory(false), resolver)
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt := app.CurrentRuntime()

	resolutions.Store(0)
	require.NoError(t, applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2)))
	require.Same(t, rt, app.CurrentRuntime(), "a unit-only change reloads in place")
	assert.Equal(t, int32(1), resolutions.Load(), "an in-place reload resolves its inputs once")

	resolutions.Store(0)
	bridgeWide := withRouteChange(inPlaceTestConfig("a", "b"), "b", 3)
	bridgeWide.Bridge.DrainTimeout = "2s"
	require.NoError(t, applyTo(t, app, bridgeWide))
	require.NotSame(t, rt, app.CurrentRuntime(), "a bridge-wide change replaces the runtime")
	assert.Equal(t, int32(1), resolutions.Load(), "the full replacement reuses what the in-place attempt resolved")
}

func TestApplyInPlace_WithdrawnAuthorizationChangesNothing(t *testing.T) {
	tf := newTrackedTransportFactory(false)
	var app *App
	var withdraw atomic.Bool
	resolver := parameterResolverFunc(func(context.Context, string) (string, error) {
		if withdraw.Load() {
			app.observationEpoch.Add(1) // the configuration is withdrawn while this apply resolves it
		}
		return "admin-secret-key-123456", nil
	})
	app = newInPlaceTestApp(t, tf, resolver)
	require.NoError(t, applyTo(t, app, inPlaceTestConfig("a", "b")))
	rt, applied := app.CurrentRuntime(), app.CurrentAppliedConfig()
	withdraw.Store(true)

	err := applyTo(t, app, withRouteChange(inPlaceTestConfig("a", "b"), "b", 2))

	require.ErrorContains(t, err, "authorization withdrawn")
	assert.Same(t, rt, app.CurrentRuntime())
	assert.True(t, rt.IsRunning())
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "nothing is retired")
	assert.Equal(t, 0, routeMaxInFlight(t, rt.Routes(), "b"))
	assert.Same(t, applied, app.CurrentAppliedConfig())
}

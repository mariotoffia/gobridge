package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// neverReadyReceiver keeps a route runner alive (Run blocks on ctx) but, as a
// ReceiverStartedSignaler whose Started() channel NEVER closes, models a route
// whose ingress never becomes ready — an isolated route fault. running closes
// once Run is entered so a test can wait for the route runner to be up.
type neverReadyReceiver struct {
	running chan struct{}
	ready   chan struct{} // never closed → route reports NOT ready
}

func newNeverReadyReceiver() *neverReadyReceiver {
	return &neverReadyReceiver{running: make(chan struct{}), ready: make(chan struct{})}
}

func (r *neverReadyReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	close(r.running)
	<-ctx.Done()
	return ctx.Err()
}

// Started satisfies ports.ReceiverStartedSignaler; the channel is never closed.
func (r *neverReadyReceiver) Started() <-chan struct{} { return r.ready }

// TestLegacyReadyBlindToRouteFault proves the legacy no-level /ready probe
// sheds traffic when an ISOLATED route is faulted. superviseRoute records the
// route fault WITHOUT flipping the global healthy flag, so the old
// running+healthy gate stayed green (200) while the bridge could not dispatch
// that route. The legacy default now requires LevelFull, so the fault yields 503,
// while the explicit ?level=connected/subscribed contract is unchanged (a route
// fault does not gate those lower rungs).
//
// Mutation check: revert handleReady's legacy branch to
// `if !rt.IsRunning() || !rt.Healthy()` and this fails — the legacy probe returns
// 200 while the route is dead (Healthy and IsRunning are both true here).
func TestLegacyReadyBlindToRouteFault(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("high3"))
	recv := newNeverReadyReceiver()
	err := rt.AddRoute(runtime.RouteConfig{
		ID: "route-faulted",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}, recv, &stubSender{}, nil, nil)
	require.NoError(t, err)

	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	// Route runner is up (receiver running) but its Started() never closes → the
	// route is permanently not-ready.
	wait.RequireClosed(t, recv.running, 2*time.Second)

	// The isolated route fault does NOT flip global health — this is exactly why
	// the old running+healthy legacy gate was blind to it.
	require.True(t, rt.IsRunning(), "an isolated route fault must not stop the runtime")
	require.True(t, rt.Healthy(), "an isolated route fault must not flip the global healthy flag")

	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	serve := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	// Legacy (no ?level=) now gates on LevelFull → 503 for the faulted route.
	legacy := serve("/api/v1/monitor/ready")
	assert.Equal(t, http.StatusServiceUnavailable, legacy.Code,
		"legacy /ready must return 503 when an isolated route is faulted")
	var legacyBody map[string]any
	require.NoError(t, json.NewDecoder(legacy.Body).Decode(&legacyBody))
	assert.Equal(t, "not ready", legacyBody["error"])

	// Explicit lower rungs are unchanged: a route fault does not gate them, so a
	// connected+subscribed bridge still reports ready at those levels.
	for _, lvl := range []string{"connected", "subscribed"} {
		rec := serve("/api/v1/monitor/ready?level=" + lvl)
		assert.Equalf(t, http.StatusOK, rec.Code,
			"?level=%s must stay 200 for an isolated route fault (contract unchanged)", lvl)
	}

	// The explicit full contract agrees with the new legacy default: 503.
	full := serve("/api/v1/monitor/ready?level=full")
	assert.Equal(t, http.StatusServiceUnavailable, full.Code)
}

// degradedRuntime is a fake ports.Runtime whose readiness derives from a crafted
// DeepHealth snapshot via the REAL ReadinessLevelFromDeepHealth, so the httpapi
// boundary exercises the production readiness projection (the code under test)
// rather than a hand-computed level. Role is "active" (a lease-holding primary)
// so the standby cap does NOT apply and the ServiceLevel check is the ONLY thing
// that can cap the level below Full — making this a true mutation sentinel for
// the BLOCKING fix. Only the methods handleReady calls are implemented; the
// embedded nil interface panics on any other call, catching accidental widening.
type degradedRuntime struct {
	ports.Runtime
	dh ports.DeepHealth
}

func (r *degradedRuntime) Role() string { return r.dh.Role }
func (r *degradedRuntime) ReadinessLevel(context.Context) ports.ReadinessLevel {
	return ports.ReadinessLevelFromDeepHealth(r.dh)
}
func (r *degradedRuntime) DeepHealth(context.Context) ports.DeepHealth { return r.dh }

// TestBlocking_LegacyReadyShedsTrafficOnDegradedSession proves the legacy
// no-level /ready probe sheds traffic when a session is connected+subscribed but
// EXPLICITLY degraded (e.g. broker flow-control blocked: publishes/acks stall).
// LevelFull is contractually "ReadyForTraffic + ServiceLevelFull", so a degraded
// session must cap the achieved level below Full and yield 503 on the default
// probe — while ?level=connected/subscribed stay 200 (a degraded session is still
// connected and subscribed).
//
// Role "active" is deliberate: a standby is already capped at LevelSubscribed by
// a separate rule, which would mask this fix. Only an active (or standalone)
// instance can reach LevelFull, so the ServiceLevel cap is the sole discriminator
// here.
//
// Mutation check: drop the ServiceLevel check in readinessLevelFromSessions and
// this fails — the level reaches Full and legacy /ready returns 200 while the
// broker connection is flow-control-blocked.
func TestBlocking_LegacyReadyShedsTrafficOnDegradedSession(t *testing.T) {
	rt := &degradedRuntime{dh: ports.DeepHealth{
		Running: true,
		Healthy: true,
		Role:    "active", // lease-holding primary → standby cap does not apply
		Sessions: []ports.SessionHealthDetail{{
			SessionID:           "s-degraded",
			Connected:           true,
			Ready:               true,
			SubscriptionsWanted: 1,
			SubscriptionsActive: 1,                          // connected + subscribed …
			ServiceLevel:        ports.ServiceLevelDegraded, // … but cannot serve
		}},
		Routes: []ports.RouteHealth{{ID: "route-degraded", Ready: true}},
	}}
	// Sanity: the projection caps a degraded active instance at Subscribed.
	require.Equal(t, ports.LevelSubscribed, ports.ReadinessLevelFromDeepHealth(rt.dh))

	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	serve := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	legacy := serve("/api/v1/monitor/ready")
	assert.Equal(t, http.StatusServiceUnavailable, legacy.Code,
		"legacy /ready must return 503 when a connected+subscribed session is degraded (BLOCKING)")

	for _, lvl := range []string{"connected", "subscribed"} {
		rec := serve("/api/v1/monitor/ready?level=" + lvl)
		assert.Equalf(t, http.StatusOK, rec.Code,
			"?level=%s must stay 200 for a degraded (but connected+subscribed) session", lvl)
	}

	full := serve("/api/v1/monitor/ready?level=full")
	assert.Equal(t, http.StatusServiceUnavailable, full.Code,
		"?level=full must return 503 for a degraded session")
}

// TestRegression_LegacyReadyGreenForStandaloneNonExclusiveBridge proves the
// legacy /ready gate (>= LevelFull) does NOT strand the canonical simple
// deployment: a standalone single-session bridge whose session is NON-exclusive
// (takes no part in lease failover). roleUnlocked classifies such an instance as
// "standalone" — per its documented contract — so the readiness cap does not pin
// it below Full and a healthy instance advertises 200.
//
// Regression: roleUnlocked previously keyed off len(sessionMgrs). A non-exclusive
// session still gets a manager (bridge_start.go:223-230) but never holds a lease,
// so the instance looked like a "standby" → capped at LevelSubscribed → legacy
// /ready 503 FOREVER for a perfectly healthy bridge — a regression the
// LevelFull gate activated.
//
// Mutation check: revert roleUnlocked to the len(sessionMgrs)-based version and
// this fails — role becomes "standby", the level caps at Subscribed, and legacy
// /ready returns 503.
func TestRegression_LegacyReadyGreenForStandaloneNonExclusiveBridge(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("standalone-nonexcl"))
	recv := newStubReceiver()
	sess := newStubSession(true, true) // connected + subscribed + ServiceLevelFull
	err := rt.AddRoute(runtime.RouteConfig{
		ID: "route-standalone",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}, recv, &stubSender{}, sess, &session.Config{SessionID: "s-standalone"}) // non-exclusive (default)
	require.NoError(t, err)

	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	wait.RequireClosed(t, recv.ready, 2*time.Second)

	// A non-exclusive session takes no part in failover → standalone, not standby.
	require.Equal(t, "standalone", rt.Role())
	require.Equal(t, ports.LevelFull, rt.ReadinessLevel(context.Background()),
		"a healthy standalone non-exclusive bridge must reach LevelFull")

	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil))
	assert.Equal(t, http.StatusOK, rec.Code,
		"legacy /ready must return 200 for a healthy standalone non-exclusive bridge (regression)")
}

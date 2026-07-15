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

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// stubSession implements ports.Session for deep health tests.
type stubSession struct {
	connected    bool
	ready        bool
	serviceLevel ports.ServiceLevel
	events       chan ports.SessionEvent
}

func newStubSession(connected, ready bool) *stubSession {
	sl := ports.ServiceLevelNone
	if connected && ready {
		sl = ports.ServiceLevelFull
	}
	return &stubSession{
		connected:    connected,
		ready:        ready,
		serviceLevel: sl,
		events:       make(chan ports.SessionEvent, 1),
	}
}

func (s *stubSession) Start(context.Context) error                               { return nil }
func (s *stubSession) Reconcile(context.Context, connectivity.SessionPlan) error { return nil }
func (s *stubSession) Health(context.Context) ports.SessionHealth {
	return ports.SessionHealth{
		Connected:                s.connected,
		Ready:                    s.ready,
		ServiceLevel:             s.serviceLevel,
		UnsettledCount:           3,
		OldestUnsettledAge:       4 * time.Second,
		ReceiveWindowUtilization: 0.75,
		RecoveryRecycleCount:     2,
	}
}
func (s *stubSession) Events() <-chan ports.SessionEvent { return s.events }
func (s *stubSession) Close(context.Context) error       { return nil }

// TestHandleDeepHealth_NotRunning validates that the deep health endpoint
// returns 503 and ReadyForTraffic=false when the runtime is not running.
func TestHandleDeepHealth_NotRunning(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("dh-not-running"))
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deephealth", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body deepHealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.False(t, body.ReadyForTraffic)
	assert.False(t, body.Running)
	assert.Equal(t, "dh-not-running", body.InstanceID)
	assert.NotNil(t, body.Sessions, "sessions should be non-nil (empty array)")
	assert.NotNil(t, body.Routes, "routes should be non-nil (empty array)")
}

// TestHandleDeepHealth_Running validates that the deep health endpoint
// returns 200 and ReadyForTraffic=true when the runtime is running
// with routes registered.
func TestHandleDeepHealth_Running(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("dh-running"))

	receiver := newStubReceiver()
	sender := &stubSender{}
	err := rt.AddRoute(runtime.RouteConfig{
		ID: "route-alpha",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}, receiver, sender, nil, nil)
	require.NoError(t, err)

	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	wait.RequireClosed(t, receiver.ready, 2*time.Second)

	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deephealth", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body deepHealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.True(t, body.ReadyForTraffic)
	assert.True(t, body.Running)
	assert.True(t, body.Healthy)
	assert.Equal(t, "dh-running", body.InstanceID)

	require.Len(t, body.Routes, 1)
	assert.Equal(t, "route-alpha", body.Routes[0].ID)
	assert.Equal(t, string(routing.DeliveryDirectHold), body.Routes[0].DeliveryMode)
}

// TestHandleDeepHealth_WithSession validates that deep health includes
// session details when the runtime has routes with sessions.
func TestHandleDeepHealth_WithSession(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("dh-session"))

	receiver := newStubReceiver()
	sender := &stubSender{}
	sess := newStubSession(true, true)
	sessCfg := &session.Config{SessionID: "sess-1"}

	err := rt.AddRoute(runtime.RouteConfig{
		ID: "route-with-session",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}, receiver, sender, sess, sessCfg)
	require.NoError(t, err)

	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	wait.RequireClosed(t, receiver.ready, 2*time.Second)

	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deephealth", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body deepHealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.True(t, body.ReadyForTraffic)

	require.Len(t, body.Sessions, 1)
	assert.Equal(t, "sess-1", body.Sessions[0].SessionID)
	assert.True(t, body.Sessions[0].Connected)
	assert.True(t, body.Sessions[0].Ready)
	assert.Equal(t, "full", body.Sessions[0].ServiceLevel)
	assert.Equal(t, 3, body.Sessions[0].UnsettledCount)
	assert.Equal(t, 4*time.Second, body.Sessions[0].OldestUnsettledAge)
	assert.Equal(t, 0.75, body.Sessions[0].ReceiveWindowUtilization)
	assert.Equal(t, uint64(2), body.Sessions[0].RecoveryRecycleCount)
	assert.Equal(t, "full", body.ServiceLevel)

	require.Len(t, body.Routes, 1)
	assert.Equal(t, "route-with-session", body.Routes[0].ID)
}

// TestHandleDeepHealth_RequiresAuth validates that the deep health
// endpoint requires monitor authentication.
func TestHandleDeepHealth_RequiresAuth(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("dh-auth"))
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deephealth", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestHandleDeepHealth_MethodNotAllowed validates that POST returns 405.
func TestHandleDeepHealth_MethodNotAllowed(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("dh-method"))
	cfg := testConfig()
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/monitor/deephealth", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

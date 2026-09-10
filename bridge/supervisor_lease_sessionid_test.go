package bridge

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLeaseSessionIDChanged covers the pure config-comparison logic that refuses
// a live reload changing a lease-bearing exclusive route's session_id — the
// lease identity that a per-process reload cannot roll safely across a cluster
// Reverting the check makes the "changed" case return nil and
// fail.
func TestLeaseSessionIDChanged(t *testing.T) {
	t.Parallel()

	route := func(routeID, sessionID string) ports.RouteDef {
		return ports.RouteDef{
			ID:      routeID,
			Session: &ports.RouteSessionDef{SessionID: sessionID},
		}
	}
	cfg := func(routes ...ports.RouteDef) *ports.BridgeConfig {
		return &ports.BridgeConfig{Routes: routes}
	}

	t.Run("session_id changed on same route -> refused", func(t *testing.T) {
		err := leaseSessionIDChanged(cfg(route("r1", "mqtt-prod")), cfg(route("r1", "mqtt-prod-v2")))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "r1")
		assert.Contains(t, err.Error(), "mqtt-prod")
		assert.Contains(t, err.Error(), "mqtt-prod-v2")
		assert.Contains(t, err.Error(), "session_id")
	})

	t.Run("session_id unchanged -> allowed", func(t *testing.T) {
		assert.NoError(t, leaseSessionIDChanged(cfg(route("r1", "mqtt-prod")), cfg(route("r1", "mqtt-prod"))))
	})

	t.Run("adding a route session is not a lease-identity change", func(t *testing.T) {
		old := cfg(ports.RouteDef{ID: "r1"}) // no session
		nw := cfg(route("r1", "mqtt-prod"))
		assert.NoError(t, leaseSessionIDChanged(old, nw))
	})

	t.Run("removing a route session is not flagged here", func(t *testing.T) {
		old := cfg(route("r1", "mqtt-prod"))
		nw := cfg(ports.RouteDef{ID: "r1"})
		assert.NoError(t, leaseSessionIDChanged(old, nw))
	})

	t.Run("different route id is not matched", func(t *testing.T) {
		// A route present only in the new config contributes no prior lease.
		assert.NoError(t, leaseSessionIDChanged(cfg(route("r1", "a")), cfg(route("r2", "b"))))
	})

	t.Run("nil configs are safe", func(t *testing.T) {
		assert.NoError(t, leaseSessionIDChanged(nil, cfg(route("r1", "a"))))
		assert.NoError(t, leaseSessionIDChanged(cfg(route("r1", "a")), nil))
	})

	// Shared-session vector: the route's exclusive lease-bearing session comes
	// from a shared sessions: block via receiver -> ReceiverDef.SessionID ->
	// SessionDef(session_mode: exclusive), NOT an inline route session. This is
	// the coverage-gap the inline-only check missed.
	shared := func(routeID, receiverID, sessionID, mode string) *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Sessions:  []ports.SessionDef{{ID: sessionID, Transport: "mqtt", SessionMode: mode}},
			Receivers: []ports.ReceiverDef{{ID: receiverID, Transport: "mqtt", SessionID: sessionID}},
			Routes:    []ports.RouteDef{{ID: routeID, ReceiverID: receiverID}},
		}
	}

	t.Run("receiver shared exclusive session_id changed -> refused", func(t *testing.T) {
		old := shared("r1", "rx1", "sess-a", "exclusive")
		nw := shared("r1", "rx1", "sess-b", "exclusive")
		err := leaseSessionIDChanged(old, nw)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "r1")
		assert.Contains(t, err.Error(), "sess-a")
		assert.Contains(t, err.Error(), "sess-b")
		assert.Contains(t, err.Error(), "session_id")
	})

	t.Run("receiver shared exclusive session_id unchanged -> allowed", func(t *testing.T) {
		old := shared("r1", "rx1", "sess-a", "exclusive")
		nw := shared("r1", "rx1", "sess-a", "exclusive")
		assert.NoError(t, leaseSessionIDChanged(old, nw))
	})

	t.Run("receiver shared NON-exclusive session_id changed -> allowed", func(t *testing.T) {
		// session_mode is not exclusive -> no ownership lease keyed on it, so a
		// repoint is not a lease-identity change.
		old := shared("r1", "rx1", "sess-a", "shared")
		nw := shared("r1", "rx1", "sess-b", "shared")
		assert.NoError(t, leaseSessionIDChanged(old, nw))
	})
}

// TestSupervisor_LeaseSessionIDChange_RefusesReload proves the check is wired
// into the live swap path: a reload changing a lease-bearing exclusive route's
// session_id fails the swap and keeps the old runtime serving under the current
// session_id.
func TestSupervisor_LeaseSessionIDChange_RefusesReload(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))

	initial := supervisorTestConfigWithSession("r1", "sess-a")
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	changed := supervisorTestConfigWithSession("r1", "sess-b") // same route, new lease identity
	require.True(t, sendConfig(ch, changed, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "session_id changed")
	assert.Same(t, oldRt, s.Runtime(), "old runtime must keep serving under the current session_id")
}

// TestSupervisor_LeaseSessionIDChange_AllowedWithDestructiveFlag proves the
// existing WithAllowDestructiveReload escape hatch forces the session_id change
// through: the swap succeeds and the runtime is replaced.
func TestSupervisor_LeaseSessionIDChange_AllowedWithDestructiveFlag(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap), WithAllowDestructiveReload(true))

	initial := supervisorTestConfigWithSession("r1", "sess-a")
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	changed := supervisorTestConfigWithSession("r1", "sess-b")
	require.True(t, sendConfig(ch, changed, time.Second))

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error, "escape hatch must force the session_id change through")
	assert.NotSame(t, oldRt, s.Runtime(), "runtime must be swapped when forced")
}

// TestSupervisor_LeaseSessionIDUnchanged_AllowsReload proves a compatible reload
// (session_id unchanged) still swaps successfully — the check does not block
// ordinary reloads.
func TestSupervisor_LeaseSessionIDUnchanged_AllowsReload(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))

	initial := supervisorTestConfigWithSession("r1", "sess-a")
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	// Bump config version but keep the same session_id: a compatible reload.
	changed := supervisorTestConfigWithSession("r1", "sess-a")
	changed.Version = initial.Version + 1
	require.True(t, sendConfig(ch, changed, time.Second))

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error, "an unchanged session_id must not block the reload")
}

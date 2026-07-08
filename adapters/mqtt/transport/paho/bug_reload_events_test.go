package paho

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// F-1 (HIGH): Zombie session after a failed credential-rotation Reload during
// a broker outage.
//
// Reload tears the ConnectionManager down (autopaho Disconnect is terminal in
// v0.23.0) and then re-Starts. If that re-Start fails because the broker is
// down, the OLD path left the session permanently dead: with no CM there is
// no autopaho reconnect and no further SessionEvent, so the runtime manager's
// handleEvents blocks on <-events forever and superviseSession never restarts
// it (readiness red, liveness green — a K8s-invisible zombie).
//
// Fix: on a Reload re-Start failure, CLOSE the session's events channel. The
// runtime manager treats an events-channel close as a session FAILURE
// (errSessionEventsClosed) and superviseSession re-invokes Manager.Run, which
// re-Starts the session and rebuilds the CM; autopaho then reconnects once the
// broker returns. The re-Start re-materialises a FRESH events channel so
// handleEvents does not spin on a closed one, and the close is guarded so a
// later Close cannot double-close it.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_ReloadStartFailure_ClosesEvents_ThenReStartReMaterialises is the
// deterministic seam test (no broker) for F-1. It drives Start/Reload/Start
// through connectOverride:
//
//	Start #1 (dial ok)      → events channel ev1 is live.
//	Reload (dial FAILS)     → re-Start fails; F-1 CLOSES ev1 (terminal-death
//	                          signal that unblocks the runtime's handleEvents).
//	Start #3 (dial ok)      → re-materialises a FRESH open events channel ev2
//	                          and reconnects (Health.Connected true).
//
// Counterfactual (proven by reverting the closeEventsLocked call in Reload):
// pre-fix ev1 is NEVER closed, so wait.RequireClosed times out — exactly the
// zombie hang the runtime manager would suffer on <-events.
func TestBug_ReloadStartFailure_ClosesEvents_ThenReStartReMaterialises(t *testing.T) {
	var dialCount atomic.Int32
	var disconnects atomic.Int32
	var failDial atomic.Bool

	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "reload-events-seam",
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	s.connectOverride = func(_ context.Context) (pahoConnection, context.CancelFunc, error) {
		dialCount.Add(1)
		if failDial.Load() {
			return nil, nil, shared.ErrUnavailable.WithMessage("simulated broker down during rotation Reload")
		}
		return &fakeLiveConn{disconnects: &disconnects}, func() {}, nil
	}

	// Start #1: the session connects and exposes a live events channel.
	require.NoError(t, s.Start(context.Background()))
	require.Equal(t, int32(1), dialCount.Load())
	require.True(t, s.Health(context.Background()).Connected)
	ev1 := s.Events()

	// Reload with the broker "down": teardown succeeds, the internal re-Start
	// fails, and Reload surfaces the error.
	failDial.Store(true)
	require.Error(t, s.Reload(context.Background()),
		"Reload must surface the failed re-Start error")
	require.Equal(t, int32(2), dialCount.Load(), "Reload attempts exactly one re-Start dial")
	require.Equal(t, int32(1), disconnects.Load(), "Reload tears the old CM down once")

	// F-1: the events channel is CLOSED to signal terminal death. This is the
	// hook the runtime manager (handleEvents → errSessionEventsClosed) turns
	// into a superviseSession restart. Pre-fix this times out (the hang).
	wait.RequireClosed(t, ev1, 2*time.Second)

	// Health must NOT lie during the dead window: no CM ⇒ not connected.
	require.False(t, s.Health(context.Background()).Connected,
		"a Reload-failed session must not report Connected")

	// The supervisor's restart re-invokes Run → Start. With the broker back,
	// Start re-materialises a FRESH events channel and reconnects.
	failDial.Store(false)
	require.NoError(t, s.Start(context.Background()))
	require.Equal(t, int32(3), dialCount.Load())
	require.True(t, s.Health(context.Background()).Connected,
		"the re-Start rebuilds the CM and reconnects")

	ev2 := s.Events()
	require.True(t, ev1 != ev2, "the re-Start must hand out a FRESH events channel, not the closed one")

	// Prove ev2 is a live, OPEN channel: pushEvent (which honours eventsClosed)
	// can send, and the value is received — i.e. eventsClosed was cleared.
	s.pushEvent(ports.SessionReconnecting, nil)
	got := wait.RequireReceive(t, ev2, 2*time.Second)
	require.Equal(t, ports.SessionReconnecting, got.Type,
		"the re-materialised events channel is open and delivers events")
}

// TestBug_ReloadStartFailure_ClosedGuard_NoDoubleClosePanic asserts the
// double-close guard (F-1 correctness detail #1): after a Reload-failure has
// already closed the events channel, a subsequent Close must NOT panic
// (Close also finalises s.events). Runs the real Close path.
func TestBug_ReloadStartFailure_ClosedGuard_NoDoubleClosePanic(t *testing.T) {
	var failDial atomic.Bool
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "reload-events-guard",
	}, connectivity.SessionEphemeral, nil)

	s.connectOverride = func(_ context.Context) (pahoConnection, context.CancelFunc, error) {
		if failDial.Load() {
			return nil, nil, shared.ErrUnavailable.WithMessage("broker down")
		}
		return &fakeLiveConn{}, func() {}, nil
	}

	require.NoError(t, s.Start(context.Background()))
	ev1 := s.Events()

	failDial.Store(true)
	require.Error(t, s.Reload(context.Background()))
	wait.RequireClosed(t, ev1, 2*time.Second)

	// The Reload failure already closed events; Close must finalise safely
	// (guarded close), not double-close and panic.
	require.NotPanics(t, func() {
		_ = s.Close(context.Background())
	}, "Close after a Reload-failure events-close must not double-close/panic")
}

// TestBug_ReloadStartFailure_LiveBroker_ReconnectsAfterOutage is the F-1
// live-broker portion (Mosquitto via mqttlocal). It exercises the FULL path:
// a real connect, a Reload whose internal re-Start fails (simulated outage via
// connectOverride for that dial ONLY), the F-1 events-close, and then a
// supervisor-style re-Start that reconnects to the REAL broker once the
// "outage" clears.
//
// This is the internal-package (package paho) counterpart of the paho_test
// integration suite so it can reach the connectOverride seam that injects the
// outage; the connect and reconnect themselves are real broker round-trips.
func TestBug_ReloadStartFailure_LiveBroker_ReconnectsAfterOutage(t *testing.T) {
	url := mqttlocal.BrokerURL(t) // skips on -short / when Docker is unavailable
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s := NewSession(SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("reload-outage"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	// Real first connect (connectOverride nil ⇒ real dial).
	require.NoError(t, s.Start(ctx))
	ev1 := s.Events()
	require.True(t, s.Health(ctx).Connected, "the session connects to the live broker")

	// Inject a broker "outage" that the Reload's internal re-Start hits. The
	// override is read only by Start on this (test) goroutine, sequentially,
	// so no lock is needed (matches the other connectOverride tests).
	s.connectOverride = func(_ context.Context) (pahoConnection, context.CancelFunc, error) {
		return nil, nil, shared.ErrUnavailable.WithMessage("simulated broker outage during rotation Reload")
	}

	require.Error(t, s.Reload(ctx), "Reload's re-Start fails during the outage")

	// F-1: the events channel closes → the runtime manager would restart.
	// Pre-fix this times out (the permanent zombie).
	wait.RequireClosed(t, ev1, 5*time.Second)

	// The "outage" clears: drop the override so the supervisor-style re-Start
	// uses the REAL dial again.
	s.connectOverride = nil

	require.NoError(t, s.Start(ctx), "re-Start dials the real broker after the outage")

	// The re-Start re-materialised a fresh events channel and reconnected to
	// the live broker.
	ev2 := s.Events()
	require.True(t, ev1 != ev2, "the re-Start hands out a fresh events channel")
	wait.Until(t, 5*time.Second, "session reconnects to the live broker after the outage", func() bool {
		return s.Health(ctx).Connected
	})
}

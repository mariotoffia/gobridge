// ═══════════════════════════════════════════════
// Production-readiness remediation tests: session lifecycle (F3b/F4/F5).
//
//   - F5: a failed INITIAL reconcile (Start with an installed plan) must
//     fail Start loudly instead of returning nil with an unbound queue —
//     silently unroutable messages with only a Degraded service level as
//     evidence.
//   - F3b: a failed reconcile during RECONNECT must not flip the session
//     to connected nor emit SessionConnected (the receiver's health probe
//     would win the race, consume from a missing queue, and die on a
//     permanent 404); the reconnect retries with backoff and heals.
//   - F4: Start/Close and doReconnect/Close races must not install a
//     connection on a closed session — that leaks a live TCP connection
//     (plus ghost consumers) until process exit.
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// subscriptionPlanForTest returns a minimal plan whose reconcile requires
// a working channel (declare exchange+queue+bind), so a failing
// Channel() makes reconcile fail deterministically.
func subscriptionPlanForTest() connectivity.SessionPlan {
	return connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{
				Topic: "orders.inbound",
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:   "orders",
					RoutingKey: "orders.#",
				}},
			},
		},
	}
}

// TestSession_Start_ReconcileFailure_FailsStart pins F5: when the initial
// reconcile fails, Start must return the mapped error (not nil), close
// the dialed connection, unwind to the pre-Start state, and emit no
// SessionConnected. A subsequent Start (after the plan is fixed) succeeds.
func TestSession_Start_ReconcileFailure_FailsStart(t *testing.T) {
	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) {
		return nil, &amqp.Error{Code: 404, Reason: "NOT_FOUND - no exchange 'orders'"}
	}
	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	plan := subscriptionPlanForTest()
	s.plan = &plan

	events, unsub := s.Subscribe()
	defer unsub()

	err := s.Start(context.Background())
	require.Error(t, err, "Start must surface a failed initial reconcile")
	var be *shared.BridgeError
	require.True(t, errors.As(err, &be))
	require.Equal(t, shared.ErrCodeNotFound, be.Code)

	// Unwound: connection closed, nothing installed, not connected.
	require.Equal(t, 1, mc.closeCalls(), "the dialed connection must be closed on reconcile failure")
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	require.Nil(t, conn, "no connection may remain installed after a failed Start")
	require.False(t, s.Health(context.Background()).Connected)

	select {
	case ev := <-events:
		t.Fatalf("no session event may be emitted for a failed Start, got %v", ev.Type)
	default:
	}

	// Start is retryable: with a heal (empty plan — nothing to declare)
	// the same session starts cleanly and emits SessionConnected.
	s.mu.Lock()
	s.plan = &connectivity.SessionPlan{}
	s.mu.Unlock()
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	require.Eventually(t, func() bool {
		select {
		case ev := <-events:
			return ev.Type == ports.SessionConnected
		default:
			return false
		}
	}, 2*time.Second, time.Millisecond, "successful retry must emit SessionConnected")
	require.True(t, s.Health(context.Background()).Connected)
}

// TestSession_StartCloseRace_ClosesDialedConnection pins F4 (Start/Close):
// when Close runs while Start's dial is in flight, the freshly dialed
// connection must be closed instead of installed on the closed session.
// Run with -race.
func TestSession_StartCloseRace_ClosesDialedConnection(t *testing.T) {
	mc := newMockConnection()
	dialEntered := make(chan struct{})
	dialRelease := make(chan struct{})
	s := newResilienceSession(func(string) (amqpConnection, error) {
		close(dialEntered)
		<-dialRelease
		return mc, nil
	})

	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()

	<-dialEntered
	require.NoError(t, s.Close(context.Background()))
	close(dialRelease)

	err := <-startErr
	require.Error(t, err, "Start must fail when the session was closed mid-dial")
	var be *shared.BridgeError
	require.True(t, errors.As(err, &be))
	require.Equal(t, shared.ErrCodeUnavailable, be.Code)

	require.Eventually(t, func() bool { return mc.closeCalls() >= 1 },
		2*time.Second, time.Millisecond,
		"the connection dialed during the race must be closed, not leaked")
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	require.Nil(t, conn)
}

// TestSession_ReconnectCloseRace_ClosesDialedConnection pins F4
// (doReconnect/Close): when Close runs while the reconnect redial is in
// flight, the redialed connection must be closed instead of installed —
// previously it lived (with any ghost consumers) until process exit.
// Run with -race.
func TestSession_ReconnectCloseRace_ClosesDialedConnection(t *testing.T) {
	conn1 := newMockConnection()
	conn1.NotifyCloseChan = make(chan error, 1)
	conn2 := newMockConnection()

	redialEntered := make(chan struct{})
	redialRelease := make(chan struct{})
	var dialMu sync.Mutex
	dials := 0
	s := newResilienceSession(func(string) (amqpConnection, error) {
		dialMu.Lock()
		dials++
		n := dials
		dialMu.Unlock()
		if n == 1 {
			return conn1, nil
		}
		if n == 2 {
			close(redialEntered)
		}
		<-redialRelease
		return conn2, nil
	})

	require.NoError(t, s.Start(context.Background()))

	// Drop the connection -> reconnectLoop wakes and redials.
	conn1.NotifyCloseChan <- &amqp.Error{Code: 320, Reason: "CONNECTION_FORCED"}

	<-redialEntered
	require.NoError(t, s.Close(context.Background()))
	close(redialRelease)

	require.Eventually(t, func() bool { return conn2.closeCalls() >= 1 },
		2*time.Second, time.Millisecond,
		"the connection redialed during the race must be closed, not leaked")
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	require.Nil(t, conn)
	require.False(t, s.Health(context.Background()).Connected)
}

// TestSession_Reconnect_ReconcileFailure_NoConnectedUntilHealed pins F3b:
// after a redial whose reconcile fails, the session must NOT report
// Connected and must NOT emit SessionConnected (that is the race the
// receiver's health probe used to win, ending in a permanent 404). The
// reconnect keeps retrying with backoff — each failed attempt's
// connection is closed — and once the topology heals, SessionConnected
// is emitted and Health flips to Connected.
func TestSession_Reconnect_ReconcileFailure_NoConnectedUntilHealed(t *testing.T) {
	conn1 := newMockConnection()
	conn1.NotifyCloseChan = make(chan error, 1)

	// Every reconnect attempt gets a fresh connection whose channel
	// opens fail -> reconcile fails (subscription declare is fatal).
	var dialMu sync.Mutex
	var reconnConns []*mockConnection
	dials := 0
	s := newResilienceSession(func(string) (amqpConnection, error) {
		dialMu.Lock()
		defer dialMu.Unlock()
		dials++
		if dials == 1 {
			return conn1, nil
		}
		mc := newMockConnection()
		mc.ChannelFn = func() (*amqpChannel, error) {
			return nil, &amqp.Error{Code: 404, Reason: "NOT_FOUND - no exchange 'orders'"}
		}
		reconnConns = append(reconnConns, mc)
		return mc, nil
	})

	// Start with no plan (nothing to reconcile), then install the
	// subscription plan the reconnect will have to restore.
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	plan := subscriptionPlanForTest()
	s.mu.Lock()
	s.plan = &plan
	s.mu.Unlock()

	events, unsub := s.Subscribe()
	defer unsub()

	// Drop the connection -> reconnect loop dials + reconciles (fails).
	conn1.NotifyCloseChan <- &amqp.Error{Code: 320, Reason: "CONNECTION_FORCED"}

	// At least two failed attempts prove bounded-backoff retrying (not a
	// single shot, not termination). Their connections must be closed.
	require.Eventually(t, func() bool {
		dialMu.Lock()
		n := len(reconnConns)
		dialMu.Unlock()
		return n >= 2
	}, 5*time.Second, time.Millisecond, "reconnect must retry after a failed reconcile")

	// While reconcile keeps failing: no SessionConnected, not Connected.
	require.False(t, s.Health(context.Background()).Connected,
		"session must not report Connected while the topology is not restored")
	for drained := false; !drained; {
		select {
		case ev := <-events:
			require.NotEqual(t, ports.SessionConnected, ev.Type,
				"SessionConnected must not fire while reconcile fails")
		default:
			drained = true
		}
	}
	dialMu.Lock()
	firstFailed := reconnConns[0]
	dialMu.Unlock()
	require.Eventually(t, func() bool { return firstFailed.closeCalls() >= 1 },
		2*time.Second, time.Millisecond,
		"each failed attempt's connection must be closed, not leaked")

	// Heal the topology (empty plan -> trivially reconciled). The next
	// attempt must flip Connected and emit SessionConnected.
	s.mu.Lock()
	s.plan = &connectivity.SessionPlan{}
	s.mu.Unlock()

	require.Eventually(t, func() bool {
		for {
			select {
			case ev := <-events:
				if ev.Type == ports.SessionConnected {
					return true
				}
			default:
				return false
			}
		}
	}, 5*time.Second, time.Millisecond, "SessionConnected must fire once reconcile succeeds")
	require.True(t, s.Health(context.Background()).Connected)
}

// TestSession_Health_ActiveTopics pins F9 (code half): Health must expose
// the reconciled subscriptions as sorted ActiveTopics so HasTopic works.
func TestSession_Health_ActiveTopics(t *testing.T) {
	s := newResilienceSession(func(string) (amqpConnection, error) {
		return newMockConnection(), nil
	})
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	// activeSubs is what reconcile()/declareSubscription populate on
	// success; a successful declare needs a live broker channel, so the
	// map is seeded directly — the mapping into SessionHealth is what
	// this test pins.
	s.mu.Lock()
	s.activeSubs = map[string]bool{"zeta": true, "alpha": true}
	s.mu.Unlock()

	h := s.Health(context.Background())
	require.Equal(t, []string{"alpha", "zeta"}, h.ActiveTopics)
	require.True(t, h.HasTopic("alpha"))
	require.True(t, h.HasTopic("zeta"))
	require.False(t, h.HasTopic("nope"))
}

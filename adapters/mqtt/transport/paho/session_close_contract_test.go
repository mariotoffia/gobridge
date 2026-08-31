package paho

import (
	"context"
	"sync"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// routerParkedConn models autopaho's Disconnect: it waits for the connection
// manager loop, which waits for the Paho client, which waits for every worker
// goroutine — including the one running our publish callback. A callback parked
// inside the router is therefore released only by router.shutdown(), so
// Disconnect cannot return until the router has stopped.
type routerParkedConn struct {
	fakeLiveConn
	callbackReturned <-chan struct{}
	once             sync.Once
	entered          chan struct{}
}

func (c *routerParkedConn) Disconnect(ctx context.Context) error {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-c.callbackReturned:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestSessionClose_StopsRouterBeforeDisconnectingTheClient pins the close
// ordering with a publish callback genuinely parked in the router's dispatch
// budget: the router is stopped FIRST, which releases the callback, which lets
// the SDK Disconnect complete.
//
// Counterfactual (the pre-fix ordering): Close called cm.Disconnect before
// router.shutdown(). Disconnect waited on the parked callback, the callback
// waited on a budget release only shutdown() could give, and Close burned its
// entire context before finally stopping the router and returning the context
// error — which the session manager reads as a wedged close that retains the
// lease until TTL. Close never returns here and the wait fails.
func TestSessionClose_StopsRouterBeforeDisconnectingTheClient(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://127.0.0.1:1883"},
		ClientID:       "close-ordering",
		ReceiveMaximum: 1,
	}, connectivity.SessionPersistent, nil)

	// Hold the session's whole dispatch budget so the next QoS 1 publish has to
	// wait for capacity, with no pending QoS 0 to reclaim.
	held := &pahov5.Publish{Topic: "held", QoS: 1}
	require.True(t, s.router.reserveQueueSlot(held, held.QoS))

	callbackReturned := make(chan struct{})
	callbackEntered := make(chan struct{})
	go func() {
		defer close(callbackReturned)
		close(callbackEntered)
		s.router.enqueueDispatch(&pahov5.Publish{Topic: "parked", QoS: 1}, nil)
	}()
	<-callbackEntered

	conn := &routerParkedConn{callbackReturned: callbackReturned, entered: make(chan struct{})}
	s.mu.Lock()
	s.cm = conn
	s.connected = true
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	var closeErr error
	done := make(chan struct{})
	go func() {
		closeErr = s.Close(ctx)
		close(done)
	}()

	wait.Until(t, 2*time.Second, "Close returns without burning its deadline on a parked callback", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	require.NoError(t, closeErr, "a cooperative close must not report the context error")
	wait.RequireClosed(t, conn.entered, time.Second)
	wait.RequireClosed(t, callbackReturned, time.Second)
}

// TestSessionClose_ReportsAnAbandonedHandlerWait pins that Close tells the truth
// when its deadline expires while route handlers are still inside emit.
//
// The session manager races Close against its own ceiling and releases an
// exclusive lease as soon as Close RETURNS. A Close that silently reports
// success after abandoning that wait therefore hands the lease to a standby
// while this owner's pipeline is still settling accepted deliveries. Ingress is
// stopped either way — the router is shut down before any bounded wait — but
// the condition must be visible rather than reported as a clean close.
func TestSessionClose_ReportsAnAbandonedHandlerWait(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "close-handler-inflight",
	}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{} // disconnects immediately, so only the handler wait can fail
	s.connected = true
	s.mu.Unlock()

	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s.router.RegisterFiltered("rx", []string{"t/#"}, func(*pahov5.Publish, func() error) {
		close(entered)
		<-release // the route runner has not accepted the delivery yet
	})
	go s.router.dispatch(&pahov5.Publish{Topic: "t/1", QoS: 1}, nil)
	<-entered

	ctx, cancel := context.WithCancel(t.Context())
	var closeErr error
	done := make(chan struct{})
	go func() {
		closeErr = s.Close(ctx)
		close(done)
	}()
	cancel()

	wait.Until(t, 2*time.Second, "Close returns once its context expires", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	require.Error(t, closeErr,
		"a close that gave up on in-flight handlers must not report success")
	require.ErrorIs(t, closeErr, shared.ErrTimeout)
}

// TestSessionClose_AbandonsSettlementRecoveryCooldown pins that the rate-limit
// wait in front of a queued settlement recovery is bound to the SESSION's
// lifetime. The recovery goroutine deliberately runs on a detached context so a
// route-scoped cancellation cannot abort a recycle, which left Close with
// nothing to wake it.
//
// Counterfactual (the pre-fix detached wait): the goroutine stayed parked on the
// cooldown timer for the full rate-limit interval after the session closed, so
// the timer is still registered on the injected clock and the second wait fails.
func TestSessionClose_AbandonsSettlementRecoveryCooldown(t *testing.T) {
	clk := clocktest.New()
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "close-cooldown",
		Clock:      clk,
	}, connectivity.SessionPersistent, nil)

	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	// A recovery completed just now, so the next request must wait out the
	// minimum interval before recycling the connection again.
	s.lastRecoveryCompleted = clk.Now()
	s.mu.Unlock()

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.Until(t, 2*time.Second, "recovery is parked on the cooldown timer", func() bool {
		return clk.TimerCount() == 1
	})

	require.NoError(t, s.Close(t.Context()))

	wait.Until(t, 2*time.Second, "closing the session abandons the cooldown wait", func() bool {
		return clk.TimerCount() == 0
	})
}

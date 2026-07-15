package paho

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestSessionRecovery_RequestDegradesSynchronouslyAndCoalesces(t *testing.T) {
	clk := clocktest.New()
	var disconnects atomic.Int32
	oldConn := &fakeLiveConn{disconnects: &disconnects}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "recovery-coalesce",
		Clock:      clk,
	}, connectivity.SessionPersistent, nil)

	s.mu.Lock()
	s.cm = oldConn
	s.connected = true
	s.mu.Unlock()

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	s.SetIngressQuiescenceWaiter(func(context.Context) error {
		entered <- struct{}{}
		<-release
		return nil
	})
	dialed := make(chan struct{}, 1)
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dialed <- struct{}{}
		return &fakeLiveConn{}, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(t.Context()).ServiceLevel)
	require.NoError(t, s.requestRecovery(t.Context()))

	<-entered
	assert.Empty(t, entered, "concurrent recovery requests must share one recycle")
	close(release)
	<-dialed
	assert.Equal(t, int32(1), disconnects.Load())
}

func TestDeliveryDisposition_PersistentQoSRetryRequestsRecovery(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "delivery-recovery",
	}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()

	releaseRecovery := make(chan struct{})
	s.SetIngressQuiescenceWaiter(func(context.Context) error {
		<-releaseRecovery
		return nil
	})
	recoveryDialed := make(chan struct{}, 1)
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		recoveryDialed <- struct{}{}
		return &fakeLiveConn{}, func() {}, nil
	}

	receiver := NewReceiver("receiver-1", s, WithTopicFilters("orders/#"))
	runCtx, cancelRun := context.WithCancel(t.Context())
	t.Cleanup(cancelRun)
	deliveries := make(chan ports.Delivery, 1)
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(runCtx, func(_ context.Context, delivery ports.Delivery) error {
			deliveries <- delivery
			return nil
		})
	}()
	<-receiver.Started()

	dispatchDone := make(chan struct{})
	go func() {
		s.router.dispatch(&pahov5.Publish{Topic: "orders/1", QoS: 1}, func() error { return nil })
		close(dispatchDone)
	}()
	delivery := <-deliveries
	<-dispatchDone

	require.NoError(t, delivery.Retry(t.Context(), 0, assert.AnError))
	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(t.Context()).ServiceLevel)

	cancelRun()
	require.ErrorIs(t, <-runDone, context.Canceled)
	close(releaseRecovery)
	<-recoveryDialed
}

func TestUnsettled_CurrentEpochTracksUntilAckOrEpochChange(t *testing.T) {
	clk := clocktest.New()
	r := newRouter(nil, nil, withRouterClock(clk), withSessionTag("unsettled-session"))
	r.beginGrace()
	t.Cleanup(func() {
		r.shutdown()
		r.awaitDispatchLoop()
	})

	settleFirst := r.trackUnsettledPacket()
	clk.Advance(3 * time.Second)
	_ = r.trackUnsettledPacket()

	snapshot := r.unsettledSnapshot(4)
	assert.Equal(t, 2, snapshot.Count)
	assert.Equal(t, 3*time.Second, snapshot.OldestAge)
	assert.Equal(t, 0.5, snapshot.ReceiveWindowUtilization)

	settleFirst()
	assert.Equal(t, 1, r.unsettledSnapshot(4).Count)

	r.beginGrace()
	assert.Zero(t, r.unsettledSnapshot(4).Count, "a new connection epoch discards old packet handles")
}

func TestUnsettled_ProtocolAckClearsTrackedPacket(t *testing.T) {
	clk := clocktest.New()
	r := newRouter(nil, nil, withRouterClock(clk))
	var ackCalls atomic.Int32
	ack := r.trackAcknowledgement(func() error {
		ackCalls.Add(1)
		return nil
	})

	assert.Equal(t, 1, r.unsettledSnapshot(10).Count)
	require.NoError(t, ack())
	assert.Zero(t, r.unsettledSnapshot(10).Count)
	assert.Equal(t, int32(1), ackCalls.Load())
}

func TestUnsettled_HealthAndMetricsExposeReceiveWindowPressure(t *testing.T) {
	clk := clocktest.New()
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://127.0.0.1:1883"},
		ClientID:       "unsettled-health",
		Clock:          clk,
		ReceiveMaximum: 4,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()

	_ = s.router.trackUnsettledPacket()
	clk.Advance(2 * time.Second)
	_ = s.router.trackUnsettledPacket()

	health := s.Health(t.Context())
	assert.Equal(t, 2, health.UnsettledCount)
	assert.Equal(t, 2*time.Second, health.OldestUnsettledAge)
	assert.Equal(t, 0.5, health.ReceiveWindowUtilization)
	require.Len(t, metrics.FindEntries(MetricMQTTUnsettled), 1)
	require.Len(t, metrics.FindEntries(MetricMQTTOldestUnsettledAge), 1)
	require.Len(t, metrics.FindEntries(MetricMQTTReceiveWindowUtilization), 1)
}

type recoveryDisconnectConn struct {
	fakeLiveConn
	disconnected chan struct{}
	once         sync.Once
}

func (c *recoveryDisconnectConn) Disconnect(context.Context) error {
	c.once.Do(func() { close(c.disconnected) })
	return nil
}

func TestSessionRecovery_DrainBoundedAndCompletedAttemptsRateLimited(t *testing.T) {
	clk := clocktest.New()
	metrics := &ports.RecordingExporter{}
	firstDisconnected := make(chan struct{})
	secondDisconnected := make(chan struct{})
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "recovery-bounds",
		Clock:      clk,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = &recoveryDisconnectConn{disconnected: firstDisconnected}
	s.connected = true
	s.mu.Unlock()

	waiterEntered := make(chan struct{}, 2)
	s.SetIngressQuiescenceWaiter(func(ctx context.Context) error {
		waiterEntered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	var dials atomic.Int32
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		if dials.Add(1) == 1 {
			return &recoveryDisconnectConn{disconnected: secondDisconnected}, func() {}, nil
		}
		return &fakeLiveConn{}, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	<-waiterEntered
	select {
	case <-firstDisconnected:
		t.Fatal("recovery disconnected before its drain bound")
	default:
	}
	clk.Advance(settlementRecoveryDrainLimit - time.Second)
	select {
	case <-firstDisconnected:
		t.Fatal("recovery disconnected before five seconds")
	default:
	}
	clk.Advance(time.Second)
	<-firstDisconnected
	require.Eventually(t, func() bool {
		return len(metrics.FindEntries(MetricMQTTSessionRecoveryRecycle)) == 1
	}, time.Second, time.Millisecond)

	require.NoError(t, s.requestRecovery(t.Context()))
	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(t.Context()).ServiceLevel)
	require.Eventually(t, func() bool {
		return clk.TimerCount() > 0 || len(waiterEntered) > 0
	}, time.Second, time.Millisecond)
	assert.Empty(t, waiterEntered, "the second drain must not start inside the minimum interval")
	clk.Advance(settlementRecoveryMinInterval - time.Second)
	select {
	case <-secondDisconnected:
		t.Fatal("second completed recovery started inside the minimum interval")
	default:
	}
	clk.Advance(time.Second)
	<-waiterEntered
	clk.Advance(settlementRecoveryDrainLimit)
	<-secondDisconnected
}

func TestSessionRecovery_MissingSessionPresentFailsAndStaysDegraded(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "recovery-session-present",
	}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.recoveryPending = true
	s.recoveryNeedsSessionPresent = true
	s.mu.Unlock()
	s.connectOverrideAwaitConnectionUp = true
	var dials atomic.Int32
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials.Add(1)
		s.mu.Lock()
		generation := s.connectionGeneration
		s.mu.Unlock()
		s.handleConnectionUpGenerationWithSessionPresent(generation, false)
		return &fakeLiveConn{}, func() {}, nil
	}

	err := s.Start(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrUnavailable)
	health := s.Health(t.Context())
	assert.NotEqual(t, ports.ServiceLevelFull, health.ServiceLevel)
	assert.Error(t, health.LastError)

	secondErr := s.Start(t.Context())
	require.Error(t, secondErr)
	assert.Equal(t, int32(1), dials.Load(), "lost broker state must remain failed until this Session instance is rebuilt")
}

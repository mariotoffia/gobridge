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
	"github.com/mariotoffia/gobridge/testutil/wait"
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
	require.NoError(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}))
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
	require.NoError(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}))
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

func TestSessionRecovery_DrainTimeoutTerminatesAndDisconnects(t *testing.T) {
	clk := clocktest.New()
	metrics := &ports.RecordingExporter{}
	disconnected := make(chan struct{})
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "recovery-drain-timeout",
		Clock:      clk,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = &recoveryDisconnectConn{disconnected: disconnected}
	s.connected = true
	s.mu.Unlock()
	events := s.Events()

	waiterEntered := make(chan struct{}, 2)
	s.SetIngressQuiescenceWaiter(func(ctx context.Context) error {
		waiterEntered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})

	require.NoError(t, s.requestRecovery(t.Context()))
	<-waiterEntered
	clk.Advance(settlementRecoveryDrainLimit - time.Nanosecond)
	select {
	case <-disconnected:
		t.Fatal("recovery disconnected before its five-second drain bound")
	default:
	}
	clk.Advance(time.Nanosecond)
	wait.RequireReceive(t, waiterEntered, time.Second)
	clk.Advance(s.recoveryAttemptTimeout())
	<-disconnected
	wait.RequireClosed(t, events, time.Second)
	assert.Empty(t, metrics.FindEntries(MetricMQTTSessionRecoveryRecycle))
	health := s.Health(t.Context())
	assert.NotEqual(t, ports.ServiceLevelFull, health.ServiceLevel)
	assert.Error(t, health.LastError)
	assert.ErrorIs(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}), shared.ErrTransportClosedPermanently)
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

type serializedReloadConn struct {
	fakeLiveConn
	disconnectEntered chan struct{}
	releaseDisconnect chan struct{}
	once              sync.Once
}

func (c *serializedReloadConn) Disconnect(ctx context.Context) error {
	c.once.Do(func() { close(c.disconnectEntered) })
	select {
	case <-c.releaseDisconnect:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSessionRecovery_CredentialReloadSharesCancelableSerializationGate(t *testing.T) {
	disconnectEntered := make(chan struct{})
	releaseDisconnect := make(chan struct{})
	s := NewSession(SessionOptions{
		BrokerURLs:                []string{"tcp://127.0.0.1:1883"},
		ClientID:                  "credential-recovery-gate",
		Username:                  "old",
		Password:                  shared.NewSecret("old"),
		AllowPlaintextCredentials: true,
	}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &serializedReloadConn{
		disconnectEntered: disconnectEntered,
		releaseDisconnect: releaseDisconnect,
	}
	s.connected = true
	s.mu.Unlock()

	dialed := make(chan struct{}, 3)
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dialed <- struct{}{}
		return &fakeLiveConn{}, func() {}, nil
	}
	credentialDone := make(chan error, 1)
	go func() {
		password := connectivity.NewPasswordCredential("new", "new")
		credentialDone <- s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(&password, nil))
	}()
	<-disconnectEntered

	gateWait := make(chan struct{}, 2)
	s.reloadGateWaitHook = func() { gateWait <- struct{}{} }
	require.NoError(t, s.requestRecovery(t.Context()))
	<-gateWait
	select {
	case <-dialed:
		t.Fatal("settlement recovery overlapped credential-triggered reload")
	default:
	}

	waitCtx, cancelWait := context.WithCancel(t.Context())
	waitingReload := make(chan error, 1)
	go func() { waitingReload <- s.Reload(waitCtx) }()
	<-gateWait
	cancelWait()
	require.ErrorIs(t, <-waitingReload, context.Canceled)

	close(releaseDisconnect)
	require.NoError(t, <-credentialDone)
	<-dialed
	<-dialed
}

type contextBlockedDisconnectConn struct {
	fakeLiveConn
	entered chan struct{}
	exited  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *contextBlockedDisconnectConn) Disconnect(ctx context.Context) error {
	c.once.Do(func() { close(c.entered) })
	defer close(c.exited)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return nil
	}
}

type recoveryCountExporter struct {
	*ports.RecordingExporter
	recycled chan struct{}
	once     sync.Once
}

func (m *recoveryCountExporter) Counter(name string, value int64, tags ...shared.Tag) {
	m.RecordingExporter.Counter(name, value, tags...)
	if name == MetricMQTTSessionRecoveryRecycle {
		m.once.Do(func() { close(m.recycled) })
	}
}

func TestSessionRecovery_BlockedDisconnectHonorsCompleteAttemptBound(t *testing.T) {
	clk := clocktest.New()
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	blocked := &contextBlockedDisconnectConn{
		entered: make(chan struct{}),
		exited:  make(chan struct{}),
		release: make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "recovery-hard-bound",
		Clock:            clk,
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = blocked
	s.connected = true
	s.mu.Unlock()
	s.connectOverride = func(ctx context.Context) (pahoConnection, context.CancelFunc, error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return &fakeLiveConn{}, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	<-blocked.entered
	attemptBound := (Config{Session: s.opts}).PostAcquireActivationTiming(s.mode).WorstCaseDuration
	clk.Advance(attemptBound)
	t.Cleanup(func() {
		select {
		case <-blocked.exited:
		default:
			close(blocked.release)
		}
	})
	wait.RequireClosed(t, blocked.exited, time.Second)
	<-metrics.recycled
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()
	health := s.Health(t.Context())
	assert.NotEqual(t, ports.ServiceLevelFull, health.ServiceLevel)
	assert.Error(t, health.LastError)
}

var _ ports.MetricsExporter = (*recoveryCountExporter)(nil)

func TestSessionRecovery_CompletionPublishesRateLimitBeforeConcurrentRequest(t *testing.T) {
	clk := clocktest.New()
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "recovery-completion-boundary",
		Clock:            clk,
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()

	settlementDrain := make(chan struct{}, 3)
	s.SetIngressQuiescenceWaiter(func(context.Context) error {
		settlementDrain <- struct{}{}
		return nil
	})
	callbackWindow := make(chan struct{})
	releaseFirstDial := make(chan struct{})
	var dials atomic.Int32
	s.connectOverrideAwaitConnectionUp = true
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		s.mu.Lock()
		generation := s.connectionGeneration
		s.mu.Unlock()
		s.handleConnectionUpGenerationWithSessionPresent(generation, true)
		if dials.Add(1) == 1 {
			close(callbackWindow)
			<-releaseFirstDial
		}
		return &fakeLiveConn{}, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	<-settlementDrain
	<-callbackWindow
	s.mu.Lock()
	firstGeneration := s.recoveryGeneration
	s.mu.Unlock()

	require.NoError(t, s.requestRecovery(t.Context()))
	s.mu.Lock()
	concurrentGeneration := s.recoveryGeneration
	s.mu.Unlock()
	assert.Equal(t, firstGeneration, concurrentGeneration,
		"a request in the connection-up/completion window must coalesce")

	close(releaseFirstDial)
	require.NoError(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}))
	assert.Equal(t, uint64(1), s.Health(t.Context()).RecoveryRecycleCount)

	clk.Advance(settlementRecoveryMinInterval - time.Nanosecond)
	require.NoError(t, s.requestRecovery(t.Context()))
	s.mu.Lock()
	boundaryGeneration := s.recoveryGeneration
	s.mu.Unlock()
	assert.Equal(t, firstGeneration+1, boundaryGeneration)
	select {
	case <-settlementDrain:
		t.Fatal("next recovery began before the exact minimum-interval boundary")
	default:
	}

	clk.Advance(time.Nanosecond)
	<-settlementDrain
	require.NoError(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}))
	assert.Equal(t, uint64(2), s.Health(t.Context()).RecoveryRecycleCount)
}

func TestSessionRecovery_SessionPresentEvidenceRejectsStaleConnectionEpoch(t *testing.T) {
	s := NewSession(SessionOptions{ClientID: "stale-session-present"}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.connEpoch = 10
	s.recoveryPending = true
	s.recoveryNeedsSessionPresent = true
	s.recoveryAttemptActive = true
	s.recoveryGeneration = 1
	generation := s.connectionGeneration
	s.mu.Unlock()

	s.handleConnectionUpGenerationWithSessionPresent(generation, true)
	s.mu.Lock()
	s.connEpoch++
	s.mu.Unlock()
	s.handleConnectionUpGenerationWithSessionPresent(generation, false)

	err := s.captureRecoveryTargetEpoch(1)
	require.Error(t, err)
	assert.NotEqual(t, ports.ServiceLevelFull, s.Health(t.Context()).ServiceLevel)
}

func TestSessionRecovery_SessionPresentEvidenceAcceptsExactConnectionEpoch(t *testing.T) {
	s := NewSession(SessionOptions{ClientID: "exact-session-present"}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.connEpoch = 20
	s.recoveryPending = true
	s.recoveryNeedsSessionPresent = true
	s.recoveryAttemptActive = true
	s.recoveryGeneration = 2
	generation := s.connectionGeneration
	s.mu.Unlock()

	s.handleConnectionUpGenerationWithSessionPresent(generation, true)
	require.NoError(t, s.captureRecoveryTargetEpoch(2))
	require.NoError(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}))
	assert.False(t, s.recoveryPending)
}

type singleGateReconcileConn struct {
	fakeLiveConn
	calls         atomic.Int32
	active        atomic.Int32
	maxActive     atomic.Int32
	firstEntered  chan struct{}
	releaseFirst  chan struct{}
	secondEntered chan struct{}
}

func (c *singleGateReconcileConn) Unsubscribe(context.Context, []string) ([]byte, error) {
	return []byte{0}, nil
}

func (c *singleGateReconcileConn) Subscribe(ctx context.Context, subs []subscribeSpec) ([]byte, error) {
	call := c.calls.Add(1)
	active := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		current := c.maxActive.Load()
		if active <= current || c.maxActive.CompareAndSwap(current, active) {
			break
		}
	}
	switch call {
	case 1:
		close(c.firstEntered)
		select {
		case <-c.releaseFirst:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case 2:
		close(c.secondEntered)
	}
	reasons := make([]byte, len(subs))
	for i := range subs {
		reasons[i] = subs[i].QoS
	}
	return reasons, nil
}

func TestSessionSerialization_OrdinaryReconcileOwnsOnlyGate(t *testing.T) {
	clk := clocktest.New()
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	conn := &singleGateReconcileConn{
		firstEntered:  make(chan struct{}),
		releaseFirst:  make(chan struct{}),
		secondEntered: make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		ClientID:         "single-session-gate",
		Clock:            clk,
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = conn
	s.connected = true
	s.mu.Unlock()
	events := s.Events()
	planA := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "a/#", QoS: 1}}}
	planB := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "b/#", QoS: 1}}}

	firstDone := make(chan error, 1)
	go func() { firstDone <- s.Reconcile(t.Context(), planA) }()
	<-conn.firstEntered

	gateState := make(chan string, 4)
	queuedFailed := make(chan struct{})
	s.recoveryQueuedFailureHook = func() { close(queuedFailed) }
	s.reloadGateWaitHook = func() { gateState <- "wait" }
	s.reloadGateAcquiredHook = func() { gateState <- "acquired" }
	require.NoError(t, s.requestRecovery(t.Context()))
	require.Equal(t, "wait", <-gateState,
		"recovery must wait behind the ordinary reconcile gate owner")

	credentialCtx, cancelCredential := context.WithCancel(t.Context())
	credentialDone := make(chan error, 1)
	go func() { credentialDone <- s.Reload(credentialCtx) }()
	require.Equal(t, "wait", <-gateState)
	cancelCredential()
	require.ErrorIs(t, <-credentialDone, context.Canceled)

	clk.Advance(s.recoveryAttemptTimeout())
	<-queuedFailed
	wait.RequireClosed(t, events, time.Second)
	assert.Empty(t, metrics.FindEntries(MetricMQTTSessionRecoveryRecycle))
	assert.Zero(t, s.Health(t.Context()).RecoveryRecycleCount)

	secondDone := make(chan error, 1)
	go func() { secondDone <- s.Reconcile(t.Context(), planB) }()
	require.Equal(t, "wait", <-gateState)
	close(conn.releaseFirst)
	require.Error(t, <-firstDone)
	secondErr := <-secondDone
	require.ErrorIs(t, secondErr, shared.ErrTransportClosedPermanently)

	assert.Equal(t, int32(1), conn.calls.Load())
	assert.Equal(t, int32(1), conn.maxActive.Load())
	health := s.Health(t.Context())
	assert.NotEqual(t, ports.ServiceLevelFull, health.ServiceLevel)
	assert.Error(t, health.LastError)
}

type queuedRecoveryConn struct {
	fakeLiveConn
	disconnected chan struct{}
	once         sync.Once
}

func (c *queuedRecoveryConn) Disconnect(context.Context) error {
	c.once.Do(func() { close(c.disconnected) })
	return nil
}

func TestSessionRecovery_QueuedRequestPublishesAttemptOnlyAfterGate(t *testing.T) {
	clk := clocktest.New()
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	disconnected := make(chan struct{})
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "queued-recovery-publication",
		Clock:            clk,
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = &queuedRecoveryConn{disconnected: disconnected}
	s.connected = true
	s.lastRecoveryCompleted = clk.Now()
	s.mu.Unlock()
	s.connectOverrideAwaitConnectionUp = true
	dialed := make(chan struct{}, 1)
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		s.mu.Lock()
		generation := s.connectionGeneration
		s.mu.Unlock()
		s.handleConnectionUpGenerationWithSessionPresent(generation, true)
		dialed <- struct{}{}
		return &fakeLiveConn{}, func() {}, nil
	}

	require.NoError(t, s.acquireReload(t.Context()))
	gateWait := make(chan struct{}, 2)
	s.reloadGateWaitHook = func() { gateWait <- struct{}{} }
	require.NoError(t, s.requestRecovery(t.Context()))
	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(t.Context()).ServiceLevel)

	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- s.Reconcile(t.Context(), connectivity.SessionPlan{}) }()
	<-gateWait
	clk.Advance(settlementRecoveryMinInterval)
	<-gateWait
	s.releaseReload()

	require.NoError(t, <-ordinaryDone,
		"ordinary reconcile ahead of the worker must not validate queued recovery state")
	<-disconnected
	<-dialed
	<-metrics.recycled
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()
	health := s.Health(t.Context())
	assert.Equal(t, uint64(1), health.RecoveryRecycleCount)
	assert.NotEqual(t, ports.ServiceLevelDegraded, health.ServiceLevel)
	assert.NoError(t, health.LastError)
}

func TestSessionRecovery_QueuedSessionAbsentIrreversiblyFailsBeforeGate(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "queued-session-absent",
	}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	generation := s.connectionGeneration
	s.mu.Unlock()
	var dials atomic.Int32
	s.connectOverrideAwaitConnectionUp = true
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials.Add(1)
		s.mu.Lock()
		currentGeneration := s.connectionGeneration
		s.mu.Unlock()
		s.handleConnectionUpGenerationWithSessionPresent(currentGeneration, true)
		return &fakeLiveConn{}, func() {}, nil
	}
	queuedFailed := make(chan struct{})
	s.recoveryQueuedFailureHook = func() { close(queuedFailed) }

	require.NoError(t, s.acquireReload(t.Context()))
	require.NoError(t, s.requestRecovery(t.Context()))
	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(t.Context()).ServiceLevel)
	s.handleConnectionUpGenerationWithSessionPresent(generation, false)
	assert.Error(t, s.Health(t.Context()).LastError)

	s.releaseReload()
	<-queuedFailed
	s.handleConnectionUpGenerationWithSessionPresent(generation, true)
	health := s.Health(t.Context())
	assert.NotEqual(t, ports.ServiceLevelFull, health.ServiceLevel)
	assert.Error(t, health.LastError)
	assert.Zero(t, health.RecoveryRecycleCount)
	assert.Zero(t, dials.Load())
}

func TestSessionRecovery_RecycleMetricStartsOnlyAfterGate(t *testing.T) {
	clk := clocktest.New()
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	blocked := &contextBlockedDisconnectConn{
		entered: make(chan struct{}),
		exited:  make(chan struct{}),
		release: make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "recycle-metric-gate",
		Clock:            clk,
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = blocked
	s.connected = true
	s.mu.Unlock()
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return &fakeLiveConn{}, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	<-blocked.entered
	<-metrics.recycled
	assert.Equal(t, uint64(1), s.Health(t.Context()).RecoveryRecycleCount)
	close(blocked.release)
	<-blocked.exited
	require.NoError(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}))
	assert.Equal(t, uint64(1), s.Health(t.Context()).RecoveryRecycleCount)
}

func TestSessionRecovery_FailedAttemptTerminatesLifecycle(t *testing.T) {
	disconnected := make(chan struct{})
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "terminal-recovery-failure",
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &queuedRecoveryConn{disconnected: disconnected}
	s.connected = true
	s.mu.Unlock()
	events := s.Events()
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return nil, nil, shared.ErrUnavailable.WithMessage("forced recovery reconnect failure")
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	<-disconnected
	terminalEvents := 0
	for event := range events {
		if event.Type == ports.SessionError {
			terminalEvents++
			require.ErrorIs(t, event.Err, shared.ErrTransportClosedPermanently)
		}
	}
	assert.Equal(t, 1, terminalEvents)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	pending := s.recoveryPending
	active := s.recoveryAttemptActive
	terminalErr := s.terminalErr
	s.mu.Unlock()
	assert.False(t, pending)
	assert.False(t, active)
	require.Error(t, terminalErr)
	assert.ErrorIs(t, terminalErr, shared.ErrTransportClosedPermanently)
	assert.ErrorIs(t, s.requestRecovery(t.Context()), shared.ErrTransportClosedPermanently)
	assert.ErrorIs(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}), shared.ErrTransportClosedPermanently)
}

func TestSessionRecovery_ConcurrentTerminalFailuresCoalesce(t *testing.T) {
	var disconnects atomic.Int32
	s := NewSession(SessionOptions{ClientID: "terminal-coalesce"}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{disconnects: &disconnects}
	s.connected = true
	generation := s.connectionGeneration
	s.mu.Unlock()
	events := s.Events()

	require.NoError(t, s.acquireReload(t.Context()))
	require.NoError(t, s.requestRecovery(t.Context()))
	const failures = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(failures)
	for range failures {
		go func() {
			defer wg.Done()
			<-start
			s.handleConnectionUpGenerationWithSessionPresent(generation, false)
		}()
	}
	close(start)
	wg.Wait()
	terminalEvents := 0
	for event := range events {
		if event.Type == ports.SessionError {
			terminalEvents++
		}
	}
	assert.Equal(t, 1, terminalEvents)
	assert.Equal(t, int32(1), disconnects.Load())
	s.releaseReload()
}

func TestSessionRecovery_FailClosedWinnerStillCompletesUnifiedTerminalTransition(t *testing.T) {
	var disconnects atomic.Int32
	s := NewSession(SessionOptions{ClientID: "fail-closed-wins"}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{disconnects: &disconnects}
	s.connected = true
	s.recoveryPending = true
	s.recoveryAttemptActive = true
	s.recoveryGeneration = 11
	s.mu.Unlock()
	events := s.Events()
	firstCause := managedMigrationRequiredError()
	secondCause := shared.ErrUnavailable.WithMessage("later recovery finalizer")

	terminal := s.failClosed(t.Context(), firstCause)
	require.ErrorIs(t, terminal, shared.ErrTransportClosedPermanently)
	assert.False(t, s.completeRecoveryAttempt(11, secondCause, false))

	terminalEvents := 0
	for event := range events {
		if event.Type == ports.SessionError {
			terminalEvents++
		}
	}
	s.mu.Lock()
	pending := s.recoveryPending
	active := s.recoveryAttemptActive
	latched := s.terminalErr
	s.mu.Unlock()
	assert.False(t, pending)
	assert.False(t, active)
	assert.Equal(t, 1, terminalEvents)
	assert.Equal(t, int32(1), disconnects.Load())
	assert.ErrorIs(t, latched, shared.ErrTransportClosedPermanently)
}

func TestSessionRecovery_SessionAbsentDuringDrainWaitsForSettlementBarrier(t *testing.T) {
	var disconnects atomic.Int32
	s := NewSession(SessionOptions{ClientID: "absent-during-drain"}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{disconnects: &disconnects}
	s.connected = true
	generation := s.connectionGeneration
	s.mu.Unlock()
	events := s.Events()
	barrierEntered := make(chan struct{}, 2)
	releaseBarrier := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseBarrier:
		default:
			close(releaseBarrier)
		}
	})
	s.SetIngressQuiescenceWaiter(func(context.Context) error {
		barrierEntered <- struct{}{}
		<-releaseBarrier
		return nil
	})

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireReceive(t, barrierEntered, time.Second)
	s.handleConnectionUpGenerationWithSessionPresent(generation, false)
	wait.RequireReceive(t, barrierEntered, time.Second)
	select {
	case <-events:
		t.Fatal("terminal signal became observable before settlement barrier released")
	default:
	}
	assert.Zero(t, disconnects.Load())

	close(releaseBarrier)
	terminalEvents := 0
	for event := range events {
		if event.Type == ports.SessionError {
			terminalEvents++
		}
	}
	assert.Equal(t, 1, terminalEvents)
	assert.Equal(t, int32(1), disconnects.Load())
}

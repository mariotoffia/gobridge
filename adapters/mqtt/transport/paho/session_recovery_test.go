package paho

import (
	"context"
	"log/slog"
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

// TestSessionRecovery_DrainTimeoutTerminatesAndDisconnects pins the OUTER bound
// on a settlement drain that never completes: the recovery attempt budget. The
// drain has no tighter bound of its own — every settlement path the runtime
// waiter observes is already bounded by the route's own send-wedge, processor
// and store ceilings, so a bound invented here could only be SHORTER than a
// legitimate settlement and would terminalize the session for cooperative
// slowness (see TestSessionRecovery_DrainOutlastsTheAdapterReconcileBound).
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
	var quiesceCalls atomic.Int32
	s.SetIngressQuiescenceWaiter(func(ctx context.Context) error {
		quiesceCalls.Add(1)
		waiterEntered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	t.Cleanup(func() { clk.Advance(s.recoveryAttemptTimeout()) })

	require.NoError(t, s.requestRecovery(t.Context()))
	<-waiterEntered
	attemptBudget := s.recoveryAttemptTimeout()
	clk.Advance(attemptBudget - time.Nanosecond)
	select {
	case <-disconnected:
		t.Fatal("recovery disconnected before its attempt budget elapsed")
	default:
	}
	clk.Advance(time.Nanosecond)
	wait.RequireClosed(t, disconnected, time.Second)
	wait.RequireClosed(t, events, time.Second)
	assert.Equal(t, int32(1), quiesceCalls.Load())
	assert.Empty(t, metrics.FindEntries(MetricMQTTSessionRecoveryRecycle))
	health := s.Health(t.Context())
	assert.NotEqual(t, ports.ServiceLevelFull, health.ServiceLevel)
	assert.Error(t, health.LastError)
	assert.ErrorIs(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}), shared.ErrTransportClosedPermanently)
}

// TestSessionRecovery_MissingSessionPresentRecordsLossAndConnects configures
// clean_start=true, so an ordinary connect of this session would not expect a
// resume. A recovery dial always asks the broker to resume, so a missing
// Session Present is still counted as a loss — and the connection comes up.
func TestSessionRecovery_MissingSessionPresentRecordsLossAndConnects(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "recovery-session-present",
		CleanStart: true,
		Clock:      clocktest.New(),
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
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

	require.NoError(t, s.Start(t.Context()))
	s.mu.Lock()
	terminalErr := s.terminalErr
	s.mu.Unlock()
	assert.NoError(t, terminalErr)
	health := s.Health(t.Context())
	require.ErrorIs(t, health.LastError, shared.ErrNotFound,
		"the resume-lost latch explains the gap until a reconcile converges")
	assert.True(t, health.Connected)
	assert.Len(t, metrics.FindEntries(MetricMQTTSessionResumeLost), 1)
	assert.Equal(t, int32(1), dials.Load())
}

func TestSessionRecovery_SessionAbsentAfterDrainCompletesRecoveryAndRecordsLoss(t *testing.T) {
	clk := clocktest.New()
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "recovery-absent-after-drain",
		Clock:            clk,
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "recovered/#", QoS: 1}},
	}
	s.mu.Unlock()
	events := s.Events()
	replacement := &captureSubConn{}
	s.connectOverrideAwaitConnectionUp = true
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		s.mu.Lock()
		generation := s.connectionGeneration
		s.mu.Unlock()
		s.handleConnectionUpGenerationWithSessionPresent(generation, false)
		return replacement, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireClosed(t, metrics.recycled, 5*time.Second)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	terminalErr := s.terminalErr
	pending := s.recoveryPending
	active := s.recoveryAttemptActive
	s.mu.Unlock()
	assert.NoError(t, terminalErr)
	assert.False(t, pending)
	assert.False(t, active)
	health := s.Health(t.Context())
	assert.Equal(t, uint64(1), health.RecoveryRecycleCount)
	assert.NoError(t, health.LastError,
		"the recovery's converged reconcile clears the resume-lost latch")
	_, resubscribed := replacement.specFor("recovered/#")
	assert.True(t, resubscribed, "the recovery reconcile re-subscribes the plan on the replacement connection")
	assert.Len(t, metrics.FindEntries(MetricMQTTSessionResumeLost), 1)
	requireNoSessionErrorBuffered(t, events)
}

// requireNoSessionErrorBuffered drains what is already buffered on events
// without blocking, and fails on a terminal signal: a SessionError or a closed
// channel.
func requireNoSessionErrorBuffered(t *testing.T, events <-chan ports.SessionEvent) {
	t.Helper()
	for {
		select {
		case event, ok := <-events:
			require.True(t, ok, "session events closed: the session went terminal")
			require.NotEqual(t, ports.SessionError, event.Type,
				"unexpected SessionError: %v", event.Err)
		default:
			return
		}
	}
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
	s.mu.Lock()
	terminalErr := s.terminalErr
	s.mu.Unlock()
	assert.NoError(t, terminalErr, "a recycle cut off by the attempt bound after the drain is abandoned, not terminal")
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

func TestSessionRecovery_RecoveryConnectionEpochRejectsStaleConnection(t *testing.T) {
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

	err := s.captureRecoveryTargetEpoch(1)
	require.Error(t, err)
	assert.NotEqual(t, ports.ServiceLevelFull, s.Health(t.Context()).ServiceLevel)
}

func TestSessionRecovery_RecoveryConnectionEpochAcceptsExactConnection(t *testing.T) {
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
	require.NoError(t, s.acquireReload(t.Context()))
	require.NoError(t, s.reconcileUnderGate(t.Context(), connectivity.SessionPlan{}, 2))
	s.releaseReload()
	s.mu.Lock()
	pending := s.recoveryPending
	s.mu.Unlock()
	assert.False(t, pending)
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

func TestSessionRecovery_QueuedSessionAbsentRecordsLossAndRecoveryContinues(t *testing.T) {
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "queued-session-absent",
		Clock:      clocktest.New(),
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
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

	require.NoError(t, s.acquireReload(t.Context()))
	require.NoError(t, s.requestRecovery(t.Context()))
	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(t.Context()).ServiceLevel)
	s.handleConnectionUpGenerationWithSessionPresent(generation, false)
	assert.ErrorIs(t, s.Health(t.Context()).LastError, shared.ErrNotFound, "the lost resume is recorded")

	s.releaseReload()
	wait.RequireClosed(t, metrics.recycled, 5*time.Second)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	terminalErr := s.terminalErr
	s.mu.Unlock()
	health := s.Health(t.Context())
	assert.Equal(t, int32(1), dials.Load())
	assert.Equal(t, uint64(1), health.RecoveryRecycleCount)
	assert.NoError(t, health.LastError)
	assert.Len(t, metrics.FindEntries(MetricMQTTSessionResumeLost), 1)
	assert.NoError(t, terminalErr)
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

// TestSessionRecovery_ReconnectFailureAfterDrainAbandonsRecovery pins that a
// recycle whose replacement dial fails, after the drain finished, is not
// terminal: the events channel closes as the ordinary dead-session signal,
// with no SessionError, and the runtime manager's re-run of this
// non-exclusive session starts it again on a fresh, open events channel.
func TestSessionRecovery_ReconnectFailureAfterDrainAbandonsRecovery(t *testing.T) {
	disconnected := make(chan struct{})
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "abandoned-recovery-reconnect",
		Clock:            clocktest.New(),
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	s.mu.Lock()
	s.cm = &queuedRecoveryConn{disconnected: disconnected}
	s.connected = true
	s.mu.Unlock()
	events := s.Events()
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return nil, nil, shared.ErrUnavailable.WithMessage("forced recovery reconnect failure")
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireClosed(t, disconnected, 5*time.Second)
	requireEventsClosedWithoutSessionError(t, events, 5*time.Second)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	pending := s.recoveryPending
	active := s.recoveryAttemptActive
	terminalErr := s.terminalErr
	s.mu.Unlock()
	assert.False(t, pending)
	assert.False(t, active)
	assert.NoError(t, terminalErr)
	assert.NotErrorIs(t, s.Reconcile(t.Context(), connectivity.SessionPlan{}), shared.ErrTransportClosedPermanently)

	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return &fakeLiveConn{}, func() {}, nil
	}
	require.NoError(t, s.Start(t.Context()), "the runtime manager's re-run starts the session again")
	requireNoSessionErrorBuffered(t, s.Events())
}

// requireEventsClosedWithoutSessionError drains events until the channel
// closes, failing the test on a SessionError or when it is still open after
// deadline.
func requireEventsClosedWithoutSessionError(t *testing.T, events <-chan ports.SessionEvent, deadline time.Duration) {
	t.Helper()
	wait.Until(t, deadline, "session events channel closed", func() bool {
		for {
			select {
			case event, ok := <-events:
				if !ok {
					return true
				}
				require.NotEqual(t, ports.SessionError, event.Type,
					"an abandoned recovery must not signal a terminal session: %v", event.Err)
			default:
				return false
			}
		}
	})
}

// TestSessionRecovery_AbandonWhileStartInFlightKeepsEventsOpen pins that a
// recovery abandoned while a Start is still dialing leaves the events channel
// open. That Start installs its connection after the abandon, so every later
// Start returns early without re-creating a closed channel: its
// SessionConnected is the only signal the runtime manager gets.
func TestSessionRecovery_AbandonWhileStartInFlightKeepsEventsOpen(t *testing.T) {
	clk := clocktest.New()
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "abandon-during-start",
		Clock:            clk,
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	events := s.Events()

	dialEntered := make(chan struct{}, 1)
	releaseDial := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseDial:
		default:
			close(releaseDial)
		}
	})
	s.connectOverrideAwaitConnectionUp = true
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		s.mu.Lock()
		generation := s.connectionGeneration
		s.mu.Unlock()
		select {
		case dialEntered <- struct{}{}:
		default:
		}
		<-releaseDial
		s.handleConnectionUpGenerationWithSessionPresent(generation, true)
		return &fakeLiveConn{}, func() {}, nil
	}
	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(t.Context()) }()
	wait.RequireReceive(t, dialEntered, 5*time.Second)

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireClosed(t, metrics.recycled, 5*time.Second)
	// The recovery's reload waits for the in-flight Start; expiring the attempt
	// budget makes it give up before any teardown, so the attempt is abandoned
	// with no connection installed.
	clk.Advance(s.recoveryAttemptTimeout())
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()
	s.mu.Lock()
	pending := s.recoveryPending
	terminalErr := s.terminalErr
	s.mu.Unlock()
	require.False(t, pending, "the recovery attempt must have ended")
	require.NoError(t, terminalErr)

	close(releaseDial)
	require.NoError(t, wait.RequireReceive(t, startDone, 5*time.Second))
	select {
	case event, ok := <-events:
		require.True(t, ok, "the events channel was closed under an in-flight Start")
		assert.Equal(t, ports.SessionConnected, event.Type)
	default:
		t.Fatal("the in-flight Start's SessionConnected was not delivered")
	}
}

func TestSessionRecovery_ConcurrentTerminalFailuresCoalesce(t *testing.T) {
	var disconnects atomic.Int32
	s := NewSession(SessionOptions{ClientID: "terminal-coalesce"}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{disconnects: &disconnects}
	s.connected = true
	s.mu.Unlock()
	events := s.Events()
	var quiesceCalls atomic.Int32
	s.SetIngressQuiescenceWaiter(func(context.Context) error {
		quiesceCalls.Add(1)
		return nil
	})

	require.NoError(t, s.acquireReload(t.Context()))
	require.NoError(t, s.requestRecovery(t.Context()))
	s.mu.Lock()
	recoveryGeneration := s.recoveryGeneration
	s.mu.Unlock()
	const failures = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(failures)
	for range failures {
		go func() {
			defer wg.Done()
			<-start
			s.terminateFailedRecovery(recoveryGeneration,
				shared.ErrUnavailable.WithMessage("forced recovery failure"), false)
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
	assert.Equal(t, int32(1), quiesceCalls.Load())
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
	assert.False(t, s.terminateFailedRecovery(11, secondCause, false))
	assert.False(t, s.abandonRecoveryAttempt(11, secondCause))

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

// newAbandonableRecoverySession returns a session with an active recovery
// attempt of generation 3 on conn, and a flag that reports whether the
// attempt's cancel func ran.
func newAbandonableRecoverySession(t *testing.T, conn pahoConnection) (*Session, *atomic.Bool) {
	t.Helper()
	s := NewSession(SessionOptions{
		ClientID: "abandon-guards",
		Clock:    clocktest.New(),
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	cancelled := &atomic.Bool{}
	s.mu.Lock()
	s.cm = conn
	s.recoveryPending = true
	s.recoveryAttemptActive = true
	s.recoveryGeneration = 3
	s.recoveryAttemptCancel = func() { cancelled.Store(true) }
	s.mu.Unlock()
	return s, cancelled
}

func requireRecoveryAttemptUntouched(t *testing.T, s *Session, cancelled *atomic.Bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.True(t, s.recoveryPending)
	require.True(t, s.recoveryAttemptActive)
	require.False(t, s.eventsClosed)
	require.False(t, cancelled.Load())
}

func TestSessionRecovery_AbandonIgnoresStaleGeneration(t *testing.T) {
	s, cancelled := newAbandonableRecoverySession(t, nil)
	cause := shared.ErrUnavailable.WithMessage("forced recovery failure after drain")

	assert.False(t, s.abandonRecoveryAttempt(2, cause), "a stale generation must not abandon the attempt")
	requireRecoveryAttemptUntouched(t, s, cancelled)
}

func TestSessionRecovery_AbandonIgnoresTerminalSession(t *testing.T) {
	s, cancelled := newAbandonableRecoverySession(t, nil)
	cause := shared.ErrUnavailable.WithMessage("forced recovery failure after drain")
	s.mu.Lock()
	s.terminalErr = shared.ErrUnavailable.Wrap(shared.ErrTransportClosedPermanently)
	s.mu.Unlock()

	assert.False(t, s.abandonRecoveryAttempt(3, cause), "a terminal session must not be abandoned back to usable")
	requireRecoveryAttemptUntouched(t, s, cancelled)
}

func TestSessionRecovery_AbandonEndsAttemptAndClosesEventsOnlyWithoutConnection(t *testing.T) {
	cases := []struct {
		name             string
		conn             pahoConnection
		wantEventsClosed bool
	}{
		{name: "no connection installed closes events", conn: nil, wantEventsClosed: true},
		{name: "installed connection keeps events open", conn: &fakeLiveConn{}, wantEventsClosed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, cancelled := newAbandonableRecoverySession(t, tc.conn)
			cause := shared.ErrUnavailable.WithMessage("forced recovery failure after drain")

			assert.True(t, s.abandonRecoveryAttempt(3, cause))

			s.mu.Lock()
			pending := s.recoveryPending
			active := s.recoveryAttemptActive
			recoveryErr := s.recoveryErr
			terminalErr := s.terminalErr
			eventsClosed := s.eventsClosed
			s.mu.Unlock()
			assert.False(t, pending)
			assert.False(t, active)
			assert.True(t, cancelled.Load())
			assert.NoError(t, recoveryErr)
			assert.NoError(t, terminalErr)
			assert.Equal(t, tc.wantEventsClosed, eventsClosed)
		})
	}
}

func TestSessionRecovery_SessionAbsentDuringDrainIsNotTerminal(t *testing.T) {
	var disconnects atomic.Int32
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "absent-during-drain",
		Clock:      clocktest.New(),
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	s.mu.Lock()
	s.cm = &fakeLiveConn{disconnects: &disconnects}
	s.connected = true
	generation := s.connectionGeneration
	s.mu.Unlock()
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return &fakeLiveConn{}, func() {}, nil
	}
	events := s.Events()
	barrierEntered := make(chan struct{}, 2)
	var quiesceCalls atomic.Int32
	releaseBarrier := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseBarrier:
		default:
			close(releaseBarrier)
		}
	})
	s.SetIngressQuiescenceWaiter(func(context.Context) error {
		quiesceCalls.Add(1)
		barrierEntered <- struct{}{}
		<-releaseBarrier
		return nil
	})

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireReceive(t, barrierEntered, time.Second)
	s.handleConnectionUpGenerationWithSessionPresent(generation, false)
	requireNoSessionErrorBuffered(t, events)
	assert.Zero(t, disconnects.Load())

	close(releaseBarrier)
	wait.RequireClosed(t, metrics.recycled, 5*time.Second)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	terminalErr := s.terminalErr
	s.mu.Unlock()
	requireNoSessionErrorBuffered(t, events)
	assert.Equal(t, int32(1), disconnects.Load())
	assert.Equal(t, int32(1), quiesceCalls.Load())
	assert.NoError(t, terminalErr)
	assert.Len(t, metrics.FindEntries(MetricMQTTSessionResumeLost), 1)
}

type terminalReconcileConn struct {
	fakeLiveConn
}

func (*terminalReconcileConn) Subscribe(context.Context, []subscribeSpec) ([]byte, error) {
	return nil, shared.ErrUnavailable.WithMessage("forced exclusive reconcile failure")
}

// TestSessionRecovery_ExclusiveRecoveryReconcileFailureAbandonsWithoutTeardown
// pins that an exclusive session whose recovery reconcile fails keeps the
// recovery connection: the recovery is abandoned without the exclusive
// teardown, and the runtime manager's next ordinary Reconcile is the one that
// tears the connection down when it fails again.
func TestSessionRecovery_ExclusiveRecoveryReconcileFailureAbandonsWithoutTeardown(t *testing.T) {
	var disconnects atomic.Int32
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "exclusive-abandoned-recovery",
		Clock:            clocktest.New(),
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionExclusive, nil, metrics)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "failed/#", QoS: 1}},
	}
	s.mu.Lock()
	s.cm = &fakeLiveConn{disconnects: &disconnects}
	s.connected = true
	s.plan = &plan
	s.mu.Unlock()
	replacement := &terminalReconcileConn{fakeLiveConn: fakeLiveConn{disconnects: &disconnects}}
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return replacement, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireClosed(t, metrics.recycled, 5*time.Second)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	terminalErr := s.terminalErr
	pending := s.recoveryPending
	active := s.recoveryAttemptActive
	current := s.cm
	eventsClosed := s.eventsClosed
	s.mu.Unlock()
	assert.NoError(t, terminalErr)
	assert.False(t, pending)
	assert.False(t, active)
	assert.Equal(t, int32(1), disconnects.Load(), "only the recycle disconnects the old connection")
	assert.Equal(t, pahoConnection(replacement), current, "the recovery connection stays installed")
	assert.False(t, eventsClosed)
	assert.Equal(t, uint64(1), s.Health(t.Context()).RecoveryRecycleCount)

	err := s.Reconcile(t.Context(), plan)
	require.Error(t, err)
	assert.NotErrorIs(t, err, shared.ErrTransportClosedPermanently)
	assert.Equal(t, int32(2), disconnects.Load(), "the ordinary exclusive reconcile failure tears the connection down")
}

// failOnceSubscribeConn rejects its first SUBSCRIBE and grants every later one
// at the requested QoS.
type failOnceSubscribeConn struct {
	fakeLiveConn
	subscribes atomic.Int32
}

func (c *failOnceSubscribeConn) Subscribe(_ context.Context, subs []subscribeSpec) ([]byte, error) {
	if c.subscribes.Add(1) == 1 {
		return nil, shared.ErrUnavailable.WithMessage("forced recovery reconcile failure")
	}
	reasons := make([]byte, len(subs))
	for i := range subs {
		reasons[i] = subs[i].QoS
	}
	return reasons, nil
}

func routerQuiesced(r *router) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.quiesced
}

// TestSessionRecovery_AbandonedRecoveryResumesRouteWorkOnNextReconcile pins
// that a recovery abandoned after its drain, with its connection installed,
// does not leave route work stopped for good: the drain quiesced the router,
// and the next ordinary Reconcile that converges releases it, dispatching the
// delivery buffered meanwhile and every later one.
func TestSessionRecovery_AbandonedRecoveryResumesRouteWorkOnNextReconcile(t *testing.T) {
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "abandoned-recovery-resumes-routes",
		Clock:            clocktest.New(),
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "orders/#", QoS: 1}},
	}
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.plan = &plan
	s.mu.Unlock()
	replacement := &failOnceSubscribeConn{}
	s.connectOverrideAwaitConnectionUp = true
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		s.mu.Lock()
		generation := s.connectionGeneration
		s.mu.Unlock()
		s.handleConnectionUpGenerationWithSessionPresent(generation, true)
		return replacement, func() {}, nil
	}
	delivered := make(chan string, 2)
	s.router.RegisterFiltered("orders", []string{"orders/#"}, func(pub *pahov5.Publish, _ func() error) {
		delivered <- pub.Topic
	})

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireClosed(t, metrics.recycled, 5*time.Second)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	terminalErr := s.terminalErr
	pending := s.recoveryPending
	current := s.cm
	s.mu.Unlock()
	require.NoError(t, terminalErr)
	require.False(t, pending)
	require.Equal(t, pahoConnection(replacement), current, "the recovery connection stays installed")
	require.Equal(t, int32(1), replacement.subscribes.Load(), "the recovery's own reconcile failed")
	require.True(t, routerQuiesced(s.router), "the abandoned recovery leaves the drain's quiescence in place")

	s.router.dispatch(&pahov5.Publish{Topic: "orders/buffered", QoS: 1}, func() error { return nil })
	select {
	case topic := <-delivered:
		t.Fatalf("delivered %q while the router was quiesced", topic)
	default:
	}

	require.NoError(t, s.Reconcile(t.Context(), plan))
	assert.False(t, routerQuiesced(s.router), "a converged ordinary reconcile releases the quiescence")
	assert.Equal(t, "orders/buffered", wait.RequireReceive(t, delivered, 5*time.Second))
	s.router.dispatch(&pahov5.Publish{Topic: "orders/live", QoS: 1}, func() error { return nil })
	assert.Equal(t, "orders/live", wait.RequireReceive(t, delivered, 5*time.Second))
}

// TestSessionRecovery_RecoveryConnectionSupersededBeforeCaptureAbandons pins
// that a recovery whose own connection is no longer the current one when the
// attempt captures its target epoch is abandoned, not terminal, and does not
// reconcile on a connection it cannot vouch for.
func TestSessionRecovery_RecoveryConnectionSupersededBeforeCaptureAbandons(t *testing.T) {
	logs := &recordingLogHandler{}
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "recovery-connection-superseded",
		Clock:            clocktest.New(),
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, slog.New(logs), metrics)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "superseded/#", QoS: 1}},
	}
	s.mu.Unlock()
	events := s.Events()
	replacement := &captureSubConn{}
	s.connectOverrideAwaitConnectionUp = true
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		s.mu.Lock()
		generation := s.connectionGeneration
		s.mu.Unlock()
		s.handleConnectionUpGenerationWithSessionPresent(generation, true)
		// The connection epoch moves past the one the recovery recorded, as
		// when a concurrent Start's connection-up lands before reloadLocked's
		// teardown bumps the epoch. Every connection-up during an attempt
		// records its own epoch, so the move is written directly.
		s.mu.Lock()
		s.connEpoch++
		s.mu.Unlock()
		return replacement, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireClosed(t, metrics.recycled, 5*time.Second)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	terminalErr := s.terminalErr
	pending := s.recoveryPending
	active := s.recoveryAttemptActive
	current := s.cm
	s.mu.Unlock()
	assert.NoError(t, terminalErr)
	assert.False(t, pending)
	assert.False(t, active)
	assert.Equal(t, pahoConnection(replacement), current)
	assert.Equal(t, 1, logs.warnCountContaining("settlement recovery abandoned"))
	_, subscribed := replacement.specFor("superseded/#")
	assert.False(t, subscribed, "an abandoned attempt must not reconcile on the superseded connection")
	requireNoSessionErrorBuffered(t, events)
}

// TestSessionRecovery_ConnectionReplacedDuringRecoveryReconcileAbandons pins
// that a recovery reconcile which converges on a connection other than the
// recovery's own is abandoned, not terminal. Managed-subscription cleanup
// recycles the connection inside that reconcile, so it returns nil on the
// replacement.
func TestSessionRecovery_ConnectionReplacedDuringRecoveryReconcileAbandons(t *testing.T) {
	logs := &recordingLogHandler{}
	metrics := &recoveryCountExporter{
		RecordingExporter: &ports.RecordingExporter{},
		recycled:          make(chan struct{}),
	}
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"old/#": {}},
	}}
	first := &managedConnFake{operations: &operations}
	recoveryConn := &managedConnFake{operations: &operations}
	replacement := &managedConnFake{operations: &operations}
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://127.0.0.1:1883"},
		ClientID:         "recovery-reconcile-replaced",
		Clock:            clocktest.New(),
		ConnectTimeout:   time.Second,
		ReconcileTimeout: time.Second,
		UnmatchedGrace:   time.Second,
	}, connectivity.SessionPersistent, slog.New(logs), metrics)
	s.managedStore = store
	s.managedIdentity = "safe-session-id"
	s.managedRequired = true
	conns := []pahoConnection{first, recoveryConn, replacement}
	var dials atomic.Int32
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dial := int(dials.Add(1))
		if dial > len(conns) {
			return nil, nil, shared.ErrUnavailable.WithMessage("unexpected dial")
		}
		return conns[dial-1], func() {}, nil
	}
	require.NoError(t, s.Start(t.Context()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	s.mu.Lock()
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "new/#", QoS: 1}},
	}
	s.mu.Unlock()
	events := s.Events()

	require.NoError(t, s.requestRecovery(t.Context()))
	wait.RequireClosed(t, metrics.recycled, 5*time.Second)
	require.NoError(t, s.acquireReload(t.Context()))
	s.releaseReload()

	s.mu.Lock()
	terminalErr := s.terminalErr
	pending := s.recoveryPending
	active := s.recoveryAttemptActive
	current := s.cm
	s.mu.Unlock()
	assert.NoError(t, terminalErr)
	assert.False(t, pending)
	assert.False(t, active)
	assert.Equal(t, int32(3), dials.Load(), "the recovery recycle and the managed cleanup recycle each dial once")
	assert.Equal(t, pahoConnection(replacement), current)
	assert.Equal(t, []string{"new/#"}, replacement.subscribed, "the reconcile converged on the replacement")
	assert.Equal(t, []string{"new/#"}, store.snapshot("safe-session-id"))
	assert.Equal(t, 1, logs.warnCountContaining("settlement recovery abandoned"))
	requireNoSessionErrorBuffered(t, events)
}

type blockedStartCleanupConn struct {
	fakeLiveConn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockedStartCleanupConn) Disconnect(context.Context) error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return nil
}

func TestSessionRecovery_TerminalSignalWaitsForStartLocalCleanup(t *testing.T) {
	conn := &blockedStartCleanupConn{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "start-local-terminal",
	}, connectivity.SessionPersistent, nil)
	s.mu.Lock()
	s.recoveryPending = true
	s.recoveryNeedsSessionPresent = true
	s.recoveryGeneration = 1
	s.mu.Unlock()
	terminalWaitingStart := make(chan struct{})
	s.terminalAwaitStartHook = func() { close(terminalWaitingStart) }
	s.connectOverrideAwaitConnectionUp = true
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		s.mu.Lock()
		generation := s.connectionGeneration
		s.mu.Unlock()
		go s.terminateFailedRecovery(1, shared.ErrUnavailable.WithMessage("forced recovery failure"), false)
		<-terminalWaitingStart
		// The connection-up barrier completes with the latched terminal error.
		s.handleConnectionUpGenerationWithSessionPresent(generation, true)
		return conn, func() {}, nil
	}
	events := s.Events()
	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(t.Context()) }()
	<-conn.entered
	<-terminalWaitingStart
	select {
	case <-events:
		t.Fatal("terminal signal preceded Start-local connection cleanup")
	default:
	}

	close(conn.release)
	require.Error(t, <-startDone)
	terminalEvents := 0
	for event := range events {
		if event.Type == ports.SessionError {
			terminalEvents++
		}
	}
	assert.Equal(t, 1, terminalEvents)
}

func TestManagedFailClosed_DeferredExclusiveTeardownDoesNotDisconnectTwice(t *testing.T) {
	var disconnects atomic.Int32
	s := NewSession(SessionOptions{ClientID: "managed-terminal-owner"}, connectivity.SessionExclusive, nil)
	s.mu.Lock()
	s.cm = &fakeLiveConn{disconnects: &disconnects}
	s.connected = true
	s.connEpoch = 30
	s.mu.Unlock()
	events := s.Events()

	firstErr := s.failClosedForManagedMigration(t.Context())
	require.ErrorIs(t, firstErr, shared.ErrTransportClosedPermanently)
	secondErr := s.disconnectFailedReconcile(t.Context())
	require.ErrorIs(t, secondErr, shared.ErrTransportClosedPermanently)

	terminalEvents := 0
	for event := range events {
		if event.Type == ports.SessionError {
			terminalEvents++
		}
	}
	s.mu.Lock()
	epoch := s.connEpoch
	s.mu.Unlock()
	assert.Equal(t, int32(1), disconnects.Load())
	assert.Equal(t, uint64(31), epoch)
	assert.Equal(t, 1, terminalEvents)
}

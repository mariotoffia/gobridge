package paho

import (
	"context"
	"sync"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type blockingReloadSubscribeConn struct {
	fakeReconcileConn

	blockMu sync.Mutex
	block   bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingReloadSubscribeConn) armSubscribeBlock() {
	c.blockMu.Lock()
	c.block = true
	c.blockMu.Unlock()
}

func (c *blockingReloadSubscribeConn) Subscribe(ctx context.Context, subs []subscribeSpec) ([]byte, error) {
	c.blockMu.Lock()
	block := c.block
	entered := c.entered
	release := c.release
	c.blockMu.Unlock()
	if block {
		c.once.Do(func() { close(entered) })
		<-release
	}
	return c.fakeReconcileConn.Subscribe(ctx, subs)
}

func TestCredentialReload_InvalidatesConvergenceBeforeDelayedConnectionUp(t *testing.T) {
	t.Run("stale Full is cleared before delayed callback", func(t *testing.T) {
		var dialCount int
		s := NewSession(SessionOptions{
			BrokerURLs:                []string{"tcp://192.0.2.1:1883"},
			ClientID:                  "reload-delayed-callback",
			Username:                  "old-user",
			Password:                  shared.NewSecret("old-password"),
			AllowPlaintextCredentials: true,
		}, connectivity.SessionEphemeral, nil)
		defer func() { _ = s.Close(context.Background()) }()
		s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
			dialCount++
			s.mu.Lock()
			s.liveCreds = mqttCredentials{Username: s.opts.Username, Password: s.opts.Password.Reveal()}
			s.mu.Unlock()
			return &fakeReconcileConn{}, func() {}, nil
		}

		require.NoError(t, s.Start(context.Background()))
		s.mu.Lock()
		s.connEpoch = 9
		s.mu.Unlock()
		plan := connectivity.SessionPlan{
			Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "reload/a", QoS: 1}},
			ExpectedReceiverIDs: []string{"rx-reload"},
		}
		s.router.Register("rx-reload", func(*pahov5.Publish) {})
		require.NoError(t, s.Reconcile(context.Background(), plan))
		require.Equal(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)

		require.NoError(t, s.ApplyCredentials(context.Background(),
			connectivity.NewCredentialSet(pwCred("new-user", "new-password"), nil)))
		require.Equal(t, 2, dialCount)

		s.mu.Lock()
		epochBeforeCallback := s.connEpoch
		satisfiedBeforeCallback := s.subscriptionsSatisfied
		s.mu.Unlock()
		require.Equal(t, uint64(10), epochBeforeCallback,
			"Reload teardown must invalidate the prior connection generation synchronously")
		require.False(t, satisfiedBeforeCallback,
			"AwaitConnection returning before OnConnectionUp must not expose stale Full")
		require.NotEqual(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)

		// Deliver the replacement connection callback after Reload has returned.
		s.handleConnectionUp()
		s.mu.Lock()
		epochAfterCallback := s.connEpoch
		satisfiedAfterCallback := s.subscriptionsSatisfied
		s.mu.Unlock()
		require.Equal(t, uint64(11), epochAfterCallback,
			"teardown invalidation and normal OnConnectionUp are distinct generations")
		require.False(t, satisfiedAfterCallback)
		require.NotEqual(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)

		require.NoError(t, s.Reconcile(context.Background(), plan))
		require.Equal(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)
	})

	t.Run("old reconcile cannot write into replacement generation", func(t *testing.T) {
		oldConn := &blockingReloadSubscribeConn{
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		newConn := &fakeReconcileConn{}
		var dialCount int
		s := NewSession(SessionOptions{
			BrokerURLs:                []string{"tcp://192.0.2.1:1883"},
			ClientID:                  "reload-stale-reconcile",
			Username:                  "old-user",
			Password:                  shared.NewSecret("old-password"),
			AllowPlaintextCredentials: true,
		}, connectivity.SessionEphemeral, nil)
		defer func() { _ = s.Close(context.Background()) }()
		s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
			dialCount++
			s.mu.Lock()
			s.liveCreds = mqttCredentials{Username: s.opts.Username, Password: s.opts.Password.Reveal()}
			s.mu.Unlock()
			if dialCount == 1 {
				return oldConn, func() {}, nil
			}
			return newConn, func() {}, nil
		}

		require.NoError(t, s.Start(context.Background()))
		s.mu.Lock()
		s.connEpoch = 20
		s.mu.Unlock()
		s.router.Register("rx-reload", func(*pahov5.Publish) {})
		planA := connectivity.SessionPlan{
			Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "reload/a", QoS: 1}},
			ExpectedReceiverIDs: []string{"rx-reload"},
		}
		require.NoError(t, s.Reconcile(context.Background(), planA))
		require.Equal(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)

		oldConn.armSubscribeBlock()
		planB := connectivity.SessionPlan{
			Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "reload/b", QoS: 1}},
			ExpectedReceiverIDs: []string{"rx-reload"},
		}
		reconcileDone := make(chan error, 1)
		go func() { reconcileDone <- s.Reconcile(context.Background(), planB) }()
		<-oldConn.entered

		reloadWaiting := make(chan struct{}, 1)
		s.reloadGateWaitHook = func() { reloadWaiting <- struct{}{} }
		credentialDone := make(chan error, 1)
		go func() {
			credentialDone <- s.ApplyCredentials(context.Background(),
				connectivity.NewCredentialSet(pwCred("new-user", "new-password"), nil))
		}()
		<-reloadWaiting
		close(oldConn.release)
		require.NoError(t, <-reconcileDone,
			"the blocked reconcile must complete before credential reload starts")
		require.NoError(t, <-credentialDone)
		require.Equal(t, 2, dialCount)

		s.mu.Lock()
		_, observedB := s.observedSubs["reload/b"]
		_, activeB := s.activeSubs["reload/b"]
		epochBeforeCallback := s.connEpoch
		satisfiedBeforeCallback := s.subscriptionsSatisfied
		s.mu.Unlock()
		require.Equal(t, uint64(21), epochBeforeCallback)
		require.False(t, observedB, "prior-generation SUBACK must not enter replacement observed state")
		require.False(t, activeB, "prior-generation SUBACK must not enter replacement active state")
		require.False(t, satisfiedBeforeCallback)
		require.NotEqual(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)

		s.handleConnectionUp()
		s.mu.Lock()
		epochAfterCallback := s.connEpoch
		s.mu.Unlock()
		require.Equal(t, uint64(22), epochAfterCallback)
		require.NoError(t, s.Reconcile(context.Background(), planB))
		require.Equal(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)
	})
}

func TestReconcile_EmptyPlanShortcutDoesNotRelatchAfterConnectionUp(t *testing.T) {
	s := NewSession(SessionOptions{
		ClientID:       "empty-shortcut-epoch",
		Clock:          testClock(),
		UnmatchedGrace: testGrace,
	}, connectivity.SessionEphemeral, nil)
	defer s.router.shutdown()
	emptyPlan := connectivity.SessionPlan{}
	s.mu.Lock()
	s.cm = &fakeReconcileConn{}
	s.connected = true
	s.connEpoch = 30
	s.appliedPlan = &emptyPlan
	s.subscriptionsSatisfied = true
	s.mu.Unlock()

	snapshotTaken := make(chan struct{})
	resume := make(chan struct{})
	s.mu.Lock()
	s.reconcileSnapshotHook = func() {
		close(snapshotTaken)
		<-resume
	}
	s.mu.Unlock()
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- s.Reconcile(context.Background(), emptyPlan) }()
	<-snapshotTaken

	// A connection-up reset landing after the empty-plan snapshot invalidates
	// that snapshot. The shortcut must not restore satisfaction for epoch 30.
	s.handleConnectionUp()
	close(resume)
	require.Error(t, <-reconcileDone)

	s.mu.Lock()
	epoch := s.connEpoch
	satisfied := s.subscriptionsSatisfied
	s.mu.Unlock()
	require.Equal(t, uint64(31), epoch)
	require.False(t, satisfied)
	require.NotEqual(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)
}

func TestReconcile_UnchangedPlanSerializesCredentialReload(t *testing.T) {
	oldConn := &fakeReconcileConn{}
	newConn := &fakeReconcileConn{}
	var dialCount int
	s := NewSession(SessionOptions{
		BrokerURLs:                []string{"tcp://192.0.2.1:1883"},
		ClientID:                  "unchanged-plan-reload",
		Username:                  "old-user",
		Password:                  shared.NewSecret("old-password"),
		AllowPlaintextCredentials: true,
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dialCount++
		s.mu.Lock()
		s.liveCreds = mqttCredentials{Username: s.opts.Username, Password: s.opts.Password.Reveal()}
		s.mu.Unlock()
		if dialCount == 1 {
			return oldConn, func() {}, nil
		}
		return newConn, func() {}, nil
	}

	require.NoError(t, s.Start(context.Background()))
	s.mu.Lock()
	s.connEpoch = 40
	s.mu.Unlock()
	s.router.Register("rx-reload", func(*pahov5.Publish) {})
	plan := connectivity.SessionPlan{
		Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "reload/unchanged", QoS: 1}},
		ExpectedReceiverIDs: []string{"rx-reload"},
	}
	require.NoError(t, s.Reconcile(context.Background(), plan))
	require.Equal(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)
	oldSubscribeCalls := oldConn.subscribeCallCount()

	outerSnapshotTaken := make(chan struct{})
	resume := make(chan struct{})
	s.mu.Lock()
	s.reconcileSnapshotHook = func() {
		close(outerSnapshotTaken)
		<-resume
	}
	s.mu.Unlock()
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- s.Reconcile(context.Background(), plan) }()
	<-outerSnapshotTaken

	// Credential reload waits behind the ordinary Reconcile gate; it cannot
	// replace cm between the outer and unchanged-plan snapshots.
	reloadWaiting := make(chan struct{}, 1)
	s.reloadGateWaitHook = func() { reloadWaiting <- struct{}{} }
	credentialDone := make(chan error, 1)
	go func() {
		credentialDone <- s.ApplyCredentials(context.Background(),
			connectivity.NewCredentialSet(pwCred("new-user", "new-password"), nil))
	}()
	<-reloadWaiting
	close(resume)
	err := <-reconcileDone
	require.NoError(t, <-credentialDone)
	require.Equal(t, 2, dialCount)
	s.mu.Lock()
	s.reconcileSnapshotHook = nil
	epoch := s.connEpoch
	satisfied := s.subscriptionsSatisfied
	s.mu.Unlock()

	require.NoError(t, err, "ordinary reconcile must finish before credential reload")
	require.Equal(t, uint64(41), epoch)
	require.False(t, satisfied)
	require.NotEqual(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)
	require.Equal(t, oldSubscribeCalls, oldConn.subscribeCallCount(),
		"unchanged stale operation must not issue another broker call on old cm")
	require.Equal(t, 0, newConn.subscribeCallCount(),
		"old operation must not pair replacement epoch/state with the new cm")
}

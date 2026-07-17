package paho

import (
	"context"
	"errors"
	"sync"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

type orphanReaddConn struct {
	fakeReconcileConn

	unsubEntered chan struct{}
	releaseUnsub chan struct{}
}

func (c *orphanReaddConn) Unsubscribe(_ context.Context, topics []string) ([]byte, error) {
	c.mu.Lock()
	c.unsubCalls++
	c.unsubTopics = append(c.unsubTopics, append([]string(nil), topics...))
	c.mu.Unlock()
	close(c.unsubEntered)
	<-c.releaseUnsub
	return make([]byte, len(topics)), nil
}

func TestOrphanUnsubscribe_SerializesWithReconcileAndCannotEraseReaddedSubscription(t *testing.T) {
	conn := &orphanReaddConn{
		unsubEntered: make(chan struct{}),
		releaseUnsub: make(chan struct{}),
	}
	s := NewSession(SessionOptions{ClientID: "orphan-readd"}, connectivity.SessionEphemeral, nil)
	s.mu.Lock()
	s.cm = conn
	s.connected = true
	s.connEpoch = 7
	// An empty APPLIED plan models "first Reconcile ran, wants nothing", so
	// readded/topic is a genuine orphan at check time. A nil plan would mean
	// no Reconcile ever ran, where every topic is covered and the orphan
	// unsubscribe is deliberately skipped (MQTT-L2).
	s.plan = &connectivity.SessionPlan{}
	s.mu.Unlock()

	orphanDone := make(chan struct{})
	go func() {
		s.unsubscribeOrphan("readded/topic")
		close(orphanDone)
	}()
	<-conn.unsubEntered

	// A broker-destructive orphan cleanup must own the same serialization gate
	// as Reconcile. The old implementation leaves this gate available here.
	serialized := true
	select {
	case <-s.reloadGate:
		serialized = false
		s.releaseReload()
	default:
	}

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "readded/topic", QoS: 1}},
	}
	// Model the public Reconcile declaration that lands after the orphan check
	// but before its blocked broker UNSUBSCRIBE returns.
	s.mu.Lock()
	s.plan = &plan
	s.mu.Unlock()

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- s.Reconcile(context.Background(), plan) }()

	var reconcileErr error
	if serialized {
		// Fixed ordering: orphan cleanup finishes first, then Reconcile subscribes.
		close(conn.releaseUnsub)
		reconcileErr = <-reconcileDone
	} else {
		// Buggy ordering: Reconcile completes first; the late orphan UNSUBSCRIBE
		// then erases the new broker subscription while observed state suppresses
		// a subsequent SUBSCRIBE.
		reconcileErr = <-reconcileDone
		close(conn.releaseUnsub)
	}
	<-orphanDone

	require.True(t, serialized, "orphan cleanup must serialize with Reconcile")
	require.NoError(t, reconcileErr)
	s.mu.Lock()
	grant, observed := s.observedSubs["readded/topic"]
	qos, active := s.activeSubs["readded/topic"]
	epoch := s.connEpoch
	s.mu.Unlock()
	require.Equal(t, uint64(7), epoch)
	require.True(t, observed)
	require.Equal(t, subscriptionGrant{Requested: 1, Granted: 1}, grant)
	require.True(t, active)
	require.Equal(t, byte(1), qos)
}

type blockingRemovalConn struct {
	fakeReconcileConn

	removalMu      sync.Mutex
	removalCalls   int
	firstEntered   chan struct{}
	releaseFirst   chan struct{}
	firstUnsubOnce sync.Once
}

func (c *blockingRemovalConn) Unsubscribe(_ context.Context, topics []string) ([]byte, error) {
	c.mu.Lock()
	c.unsubCalls++
	c.unsubTopics = append(c.unsubTopics, append([]string(nil), topics...))
	c.mu.Unlock()

	c.removalMu.Lock()
	c.removalCalls++
	call := c.removalCalls
	c.removalMu.Unlock()
	if call == 1 {
		c.firstUnsubOnce.Do(func() { close(c.firstEntered) })
		<-c.releaseFirst
		return nil, errors.New("forced unsubscribe failure")
	}
	return make([]byte, len(topics)), nil
}

func TestHealth_SubscriptionsUnsatisfiedUntilExactRemovalConverges(t *testing.T) {
	conn := &blockingRemovalConn{
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	s := NewSession(SessionOptions{ClientID: "exact-removal"}, connectivity.SessionEphemeral, nil)
	s.mu.Lock()
	s.cm = conn
	s.connected = true
	s.mu.Unlock()
	s.router.Register("rx-a", func(*pahov5.Publish) {})

	planAB := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "wanted/a", QoS: 1},
			{Topic: "remove/b", QoS: 1},
		},
		ExpectedReceiverIDs: []string{"rx-a"},
	}
	require.NoError(t, s.Reconcile(context.Background(), planAB))
	require.Equal(t, ports.ServiceLevelFull, s.Health(context.Background()).ServiceLevel)

	planA := connectivity.SessionPlan{
		Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "wanted/a", QoS: 1}},
		ExpectedReceiverIDs: []string{"rx-a"},
	}
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- s.Reconcile(context.Background(), planA) }()
	<-conn.firstEntered

	inFlight := s.Health(context.Background())
	close(conn.releaseFirst)
	err := <-reconcileDone
	require.Error(t, err)
	afterFailure := s.Health(context.Background())

	require.NotNil(t, inFlight.SubscriptionsSatisfied)
	require.False(t, *inFlight.SubscriptionsSatisfied,
		"subscription satisfaction must be false while a removal is pending")
	require.NotEqual(t, ports.ServiceLevelFull, inFlight.ServiceLevel)
	require.NotNil(t, afterFailure.SubscriptionsSatisfied)
	require.False(t, *afterFailure.SubscriptionsSatisfied,
		"a failed removal must keep the explicit plan unsatisfied despite active superset coverage")
	require.NotEqual(t, ports.ServiceLevelFull, afterFailure.ServiceLevel)

	require.NoError(t, s.Reconcile(context.Background(), planA))
	converged := s.Health(context.Background())
	require.NotNil(t, converged.SubscriptionsSatisfied)
	require.True(t, *converged.SubscriptionsSatisfied)
	require.Equal(t, ports.ServiceLevelFull, converged.ServiceLevel)
	require.Equal(t, []string{"wanted/a"}, converged.ActiveTopics)
}

func TestOrphanUnsubscribe_ClearsObservedAndActiveStateOnlyForCapturedEpoch(t *testing.T) {
	t.Run("same epoch clears downgraded observation", func(t *testing.T) {
		conn := &fakeReconcileConn{}
		s := NewSession(SessionOptions{ClientID: "orphan-clear"}, connectivity.SessionEphemeral, nil)
		s.mu.Lock()
		s.cm = conn
		s.connEpoch = 3
		s.observedSubs["orphan/topic"] = subscriptionGrant{Requested: 1, Granted: 0}
		// Reconciled empty plan: orphan handling is deferred entirely while no
		// plan has ever been stashed (MQTT-L2).
		s.plan = &connectivity.SessionPlan{}
		s.mu.Unlock()

		s.unsubscribeOrphan("orphan/topic")

		s.mu.Lock()
		_, observed := s.observedSubs["orphan/topic"]
		_, active := s.activeSubs["orphan/topic"]
		s.mu.Unlock()
		require.False(t, observed)
		require.False(t, active)
	})

	t.Run("new connection generation retains new state", func(t *testing.T) {
		s := NewSession(SessionOptions{
			ClientID:       "orphan-epoch",
			Clock:          testClock(),
			UnmatchedGrace: testGrace,
		}, connectivity.SessionEphemeral, nil)
		defer s.router.shutdown()
		conn := &epochChangingOrphanConn{session: s, topic: "orphan/topic"}
		s.mu.Lock()
		s.cm = conn
		s.connEpoch = 4
		// Reconciled empty plan: see MQTT-L2 note above.
		s.plan = &connectivity.SessionPlan{}
		s.mu.Unlock()

		s.unsubscribeOrphan("orphan/topic")

		s.mu.Lock()
		grant, observed := s.observedSubs["orphan/topic"]
		qos, active := s.activeSubs["orphan/topic"]
		epoch := s.connEpoch
		s.mu.Unlock()
		require.Equal(t, uint64(5), epoch)
		require.True(t, observed)
		require.Equal(t, subscriptionGrant{Requested: 1, Granted: 1}, grant)
		require.True(t, active)
		require.Equal(t, byte(1), qos)
	})
}

type epochChangingOrphanConn struct {
	fakeReconcileConn
	session *Session
	topic   string
}

func (c *epochChangingOrphanConn) Unsubscribe(context.Context, []string) ([]byte, error) {
	c.session.handleConnectionUp()
	c.session.mu.Lock()
	c.session.observedSubs[c.topic] = subscriptionGrant{Requested: 1, Granted: 1}
	c.session.activeSubs[c.topic] = 1
	c.session.mu.Unlock()
	return nil, nil
}

func TestHealth_ExplicitEmptyPlanUnsatisfiedAcrossReconnectUntilReconciled(t *testing.T) {
	s := NewSession(SessionOptions{
		ClientID:       "empty-plan-reconnect",
		Clock:          testClock(),
		UnmatchedGrace: testGrace,
	}, connectivity.SessionEphemeral, nil)
	defer s.router.shutdown()
	s.mu.Lock()
	s.cm = &fakeReconcileConn{}
	s.connected = true
	s.mu.Unlock()

	require.NoError(t, s.Reconcile(context.Background(), connectivity.SessionPlan{}))
	before := s.Health(context.Background())
	require.NotNil(t, before.SubscriptionsSatisfied)
	require.True(t, *before.SubscriptionsSatisfied)
	require.Equal(t, ports.ServiceLevelFull, before.ServiceLevel)

	s.handleConnectionUp()
	reconnecting := s.Health(context.Background())
	require.NotNil(t, reconnecting.SubscriptionsSatisfied)
	require.False(t, *reconnecting.SubscriptionsSatisfied)
	require.NotEqual(t, ports.ServiceLevelFull, reconnecting.ServiceLevel)

	require.NoError(t, s.Reconcile(context.Background(), connectivity.SessionPlan{}))
	after := s.Health(context.Background())
	require.NotNil(t, after.SubscriptionsSatisfied)
	require.True(t, *after.SubscriptionsSatisfied)
	require.Equal(t, ports.ServiceLevelFull, after.ServiceLevel)
}

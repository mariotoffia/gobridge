package paho

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// MQTT 3.1.1 has no Retain Handling, so every SUBSCRIBE makes the broker send
// the filter's retained messages again (MQTT 3.1.1 §3.8.4). A connection that
// comes up with Session Present = 1 resumed a session that still holds its
// subscriptions (§3.2.2.2, §3.1.2.4), so the reconcile that follows must
// subscribe only what changed. These tests pin which subscription record a
// resumed 3.1.1 connection keeps, and that every other connection still starts
// from an empty record and subscribes the whole plan.

func resumeSub(topic string, qos int) connectivity.SubscriptionPlan {
	return connectivity.SubscriptionPlan{Topic: topic, QoS: qos}
}

func resumePlan(subs ...connectivity.SubscriptionPlan) connectivity.SessionPlan {
	return connectivity.SessionPlan{Subscriptions: subs}
}

// ackLosingConn is a broker connection whose next SUBSCRIBE or UNSUBSCRIBE
// reaches the broker but whose acknowledgement never arrives, as when the
// connection drops in the middle of the round trip. Its next Disconnect can be
// made to fail.
type ackLosingConn struct {
	*fakeReconcileConn
	loseNextSubAck     atomic.Bool
	loseNextUnsubAck   atomic.Bool
	failNextDisconnect atomic.Bool
}

func (c *ackLosingConn) Disconnect(ctx context.Context) error {
	if c.failNextDisconnect.Swap(false) {
		return errors.New("connection reset during DISCONNECT")
	}
	return c.fakeReconcileConn.Disconnect(ctx)
}

func (c *ackLosingConn) Subscribe(ctx context.Context, subs []subscribeSpec) ([]byte, error) {
	reasons, err := c.fakeReconcileConn.Subscribe(ctx, subs)
	if c.loseNextSubAck.Swap(false) {
		return nil, errors.New("connection lost before SUBACK")
	}
	return reasons, err
}

func (c *ackLosingConn) Unsubscribe(ctx context.Context, topics []string) ([]byte, error) {
	reasons, err := c.fakeReconcileConn.Unsubscribe(ctx, topics)
	if c.loseNextUnsubAck.Swap(false) {
		return nil, errors.New("connection lost before UNSUBACK")
	}
	return reasons, err
}

var _ pahoConnection = (*ackLosingConn)(nil)

// newResumeTestSession builds a session on a fake clock, so no QoS probe timer
// fires on its own, over a connection that records every SUBSCRIBE and
// UNSUBSCRIBE.
func newResumeTestSession(
	t *testing.T, version string, mode connectivity.SessionMode, configure ...func(*SessionOptions),
) (*Session, *ackLosingConn) {
	t.Helper()
	opts := SessionOptions{
		ProtocolVersion: version,
		BrokerURLs:      []string{"tcp://192.0.2.1:1883"},
		ClientID:        "resume-subscriptions",
		Clock:           clocktest.NewAt(time.Unix(1_700_000_000, 0)),
	}
	for _, apply := range configure {
		apply(&opts)
	}
	s := NewSession(opts, mode, nil)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	conn := &ackLosingConn{fakeReconcileConn: &fakeReconcileConn{}}
	s.mu.Lock()
	s.cm = conn
	s.mu.Unlock()
	return s, conn
}

// connectionUp raises the edge autopaho raises when a CONNACK arrives.
func connectionUp(s *Session, sessionPresent bool) {
	s.handleConnectionUpGenerationWithSessionPresent(connectionGenerationOf(s), sessionPresent)
}

// subscribedSince returns, sorted, every filter SUBSCRIBEd after the first
// calls SUBSCRIBE packets.
func subscribedSince(conn *ackLosingConn, calls int) []string {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	var topics []string
	for _, call := range conn.subTopics[calls:] {
		topics = append(topics, call...)
	}
	sort.Strings(topics)
	return topics
}

// unsubscribedSince returns, sorted, every filter UNSUBSCRIBEd after the first
// calls UNSUBSCRIBE packets.
func unsubscribedSince(conn *ackLosingConn, calls int) []string {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	var topics []string
	for _, call := range conn.unsubTopics[calls:] {
		topics = append(topics, call...)
	}
	sort.Strings(topics)
	return topics
}

func subscriptionsSatisfied(t *testing.T, s *Session) bool {
	t.Helper()
	health := s.Health(context.Background())
	require.NotNil(t, health.SubscriptionsSatisfied)
	return *health.SubscriptionsSatisfied
}

func TestResumedMQTT311Session_SubscribesOnlyWhatChanged(t *testing.T) {
	applied := resumePlan(resumeSub("plant/a", 1), resumeSub("plant/b", 0))
	cases := []struct {
		name             string
		next             connectivity.SessionPlan
		wantSubscribed   []string
		wantUnsubscribed []string
	}{
		{
			name: "an unchanged plan subscribes nothing",
			next: applied,
		},
		{
			name:           "an added filter is subscribed alone",
			next:           resumePlan(resumeSub("plant/a", 1), resumeSub("plant/b", 0), resumeSub("plant/c", 1)),
			wantSubscribed: []string{"plant/c"},
		},
		{
			name:           "a filter whose requested QoS changed is subscribed alone",
			next:           resumePlan(resumeSub("plant/a", 2), resumeSub("plant/b", 0)),
			wantSubscribed: []string{"plant/a"},
		},
		{
			name:             "a removed filter is unsubscribed and nothing is subscribed",
			next:             resumePlan(resumeSub("plant/a", 1)),
			wantUnsubscribed: []string{"plant/b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, conn := newResumeTestSession(t, ProtocolVersion311, connectivity.SessionPersistent)
			connectionUp(s, false)
			require.NoError(t, s.Reconcile(context.Background(), applied))
			subCalls, unsubCalls := conn.subscribeCallCount(), conn.unsubscribeCallCount()

			connectionUp(s, true)
			assert.Equal(t, []string{"plant/a", "plant/b"}, s.Health(context.Background()).ActiveTopics,
				"the broker still holds the subscriptions the previous connection confirmed")
			assert.False(t, subscriptionsSatisfied(t, s),
				"readiness waits for the reconcile of the new connection, as on every connection")

			require.NoError(t, s.Reconcile(context.Background(), tc.next))
			assert.Equal(t, tc.wantSubscribed, subscribedSince(conn, subCalls))
			assert.Equal(t, tc.wantUnsubscribed, unsubscribedSince(conn, unsubCalls))
			assert.True(t, subscriptionsSatisfied(t, s), "the reconcile converges the plan")
			assert.Len(t, s.Health(context.Background()).ActiveTopics, len(tc.next.Subscriptions))
		})
	}
}

func TestConnectionUp_SubscribesTheWholePlanUnlessAnMQTT311SessionResumed(t *testing.T) {
	plan := resumePlan(resumeSub("plant/a", 1), resumeSub("plant/b", 0))
	cases := []struct {
		name           string
		version        string
		mode           connectivity.SessionMode
		sessionPresent bool
		configure      func(*SessionOptions)
	}{
		{name: "MQTT 3.1.1 without Session Present", version: ProtocolVersion311,
			mode: connectivity.SessionPersistent},
		{name: "MQTT 5 with Session Present", version: ProtocolVersion5,
			mode: connectivity.SessionPersistent, sessionPresent: true},
		{name: "MQTT 5 without Session Present", version: ProtocolVersion5,
			mode: connectivity.SessionPersistent},
		{name: "MQTT 3.1.1 ephemeral session", version: ProtocolVersion311,
			mode: connectivity.SessionEphemeral, sessionPresent: true},
		{name: "MQTT 3.1.1 over independent brokers", version: ProtocolVersion311,
			mode: connectivity.SessionPersistent, sessionPresent: true,
			configure: func(o *SessionOptions) {
				o.BrokerURLs = []string{"tcp://192.0.2.1:1883", "tcp://192.0.2.2:1883"}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var configure []func(*SessionOptions)
			if tc.configure != nil {
				configure = append(configure, tc.configure)
			}
			s, conn := newResumeTestSession(t, tc.version, tc.mode, configure...)
			connectionUp(s, false)
			require.NoError(t, s.Reconcile(context.Background(), plan))
			subCalls := conn.subscribeCallCount()

			connectionUp(s, tc.sessionPresent)
			assert.Empty(t, s.Health(context.Background()).ActiveTopics, "the record starts empty")

			require.NoError(t, s.Reconcile(context.Background(), plan))
			assert.Equal(t, []string{"plant/a", "plant/b"}, subscribedSince(conn, subCalls),
				"every filter of the plan is subscribed again")
			assert.True(t, subscriptionsSatisfied(t, s))
		})
	}
}

// A filter whose last SUBSCRIBE or UNSUBSCRIBE got no acknowledgement may or may
// not have changed on the broker, so a resumed connection cannot keep its
// record. It is subscribed again if the plan wants it, as after a fresh session.
func TestResumedMQTT311Session_ResubscribesAFilterWhoseLastOperationWasNotAcknowledged(t *testing.T) {
	t.Run("SUBSCRIBE", func(t *testing.T) {
		applied := resumePlan(resumeSub("plant/a", 1), resumeSub("plant/b", 0))
		s, conn := newResumeTestSession(t, ProtocolVersion311, connectivity.SessionPersistent)
		connectionUp(s, false)
		require.NoError(t, s.Reconcile(context.Background(), applied))

		// The broker may already hold plant/a at QoS 2 when the connection drops.
		conn.loseNextSubAck.Store(true)
		require.Error(t, s.Reconcile(context.Background(),
			resumePlan(resumeSub("plant/a", 2), resumeSub("plant/b", 0))))
		subCalls := conn.subscribeCallCount()

		connectionUp(s, true)
		require.NoError(t, s.Reconcile(context.Background(), applied))
		assert.Equal(t, []string{"plant/a"}, subscribedSince(conn, subCalls),
			"plant/a is subscribed at the QoS the plan wants again; plant/b is kept")
	})

	t.Run("UNSUBSCRIBE", func(t *testing.T) {
		applied := resumePlan(resumeSub("plant/a", 1), resumeSub("plant/b", 0))
		s, conn := newResumeTestSession(t, ProtocolVersion311, connectivity.SessionPersistent)
		connectionUp(s, false)
		require.NoError(t, s.Reconcile(context.Background(), applied))

		// The broker may already have removed plant/b when the connection drops.
		conn.loseNextUnsubAck.Store(true)
		require.Error(t, s.Reconcile(context.Background(), resumePlan(resumeSub("plant/a", 1))))
		subCalls := conn.subscribeCallCount()

		connectionUp(s, true)
		require.NoError(t, s.Reconcile(context.Background(), applied))
		assert.Equal(t, []string{"plant/b"}, subscribedSince(conn, subCalls),
			"plant/b is subscribed again; plant/a is kept")
	})
}

// A filter granted below its requested QoS is re-subscribed on every reconnect,
// as before, so the reconnect re-evaluates the grant.
func TestResumedMQTT311Session_ResubscribesAFilterGrantedBelowItsRequestedQoS(t *testing.T) {
	s, conn := newResumeTestSession(t, ProtocolVersion311, connectivity.SessionPersistent)
	conn.setTopicReasons(map[string]byte{"plant/a": 0x00})
	plan := resumePlan(resumeSub("plant/a", 1), resumeSub("plant/b", 0))
	connectionUp(s, false)
	require.NoError(t, s.Reconcile(context.Background(), plan))
	subCalls := conn.subscribeCallCount()

	connectionUp(s, true)
	require.NoError(t, s.Reconcile(context.Background(), plan))
	assert.Equal(t, []string{"plant/a"}, subscribedSince(conn, subCalls))
}

// Reload replaces the connection while the session keeps owning the broker
// session: a credential rotation, a managed-subscription cleanup, a settlement
// recovery. On MQTT 3.1.1 the replacement keeps the record when the broker
// resumed the session. A Reload that fails gives the connection up, and the
// next owner of the client id may change the broker session, so the record is
// dropped.
func TestReload_MQTT311ReplacementKeepsTheRecordOnlyWhenResumed(t *testing.T) {
	plan := resumePlan(resumeSub("plant/a", 1), resumeSub("plant/b", 0))
	reloadable := func(t *testing.T, version string) (*Session, *ackLosingConn, *atomic.Bool) {
		t.Helper()
		s, conn := newResumeTestSession(t, version, connectivity.SessionPersistent)
		var failDial atomic.Bool
		s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
			if failDial.Load() {
				return nil, nil, shared.ErrUnavailable.WithMessage("broker unreachable")
			}
			return conn, func() {}, nil
		}
		connectionUp(s, false)
		require.NoError(t, s.Reconcile(context.Background(), plan))
		return s, conn, &failDial
	}

	t.Run("MQTT 3.1.1 resumed replacement", func(t *testing.T) {
		s, conn, _ := reloadable(t, ProtocolVersion311)
		require.NoError(t, s.Reload(context.Background()))
		subCalls := conn.subscribeCallCount()
		connectionUp(s, true)
		require.NoError(t, s.Reconcile(context.Background(), plan))
		assert.Empty(t, subscribedSince(conn, subCalls))
	})

	t.Run("MQTT 3.1.1 replacement without Session Present", func(t *testing.T) {
		s, conn, _ := reloadable(t, ProtocolVersion311)
		require.NoError(t, s.Reload(context.Background()))
		subCalls := conn.subscribeCallCount()
		connectionUp(s, false)
		require.NoError(t, s.Reconcile(context.Background(), plan))
		assert.Equal(t, []string{"plant/a", "plant/b"}, subscribedSince(conn, subCalls))
	})

	t.Run("MQTT 5 resumed replacement", func(t *testing.T) {
		s, conn, _ := reloadable(t, ProtocolVersion5)
		require.NoError(t, s.Reload(context.Background()))
		subCalls := conn.subscribeCallCount()
		connectionUp(s, true)
		require.NoError(t, s.Reconcile(context.Background(), plan))
		assert.Equal(t, []string{"plant/a", "plant/b"}, subscribedSince(conn, subCalls))
	})

	t.Run("MQTT 3.1.1 failed Reload", func(t *testing.T) {
		s, conn, failDial := reloadable(t, ProtocolVersion311)
		failDial.Store(true)
		require.Error(t, s.Reload(context.Background()))
		assert.Empty(t, s.Health(context.Background()).ActiveTopics, "the failed Reload dropped the record")

		failDial.Store(false)
		require.NoError(t, s.Start(context.Background()), "the supervisor starts the session again")
		subCalls := conn.subscribeCallCount()
		connectionUp(s, true)
		require.NoError(t, s.Reconcile(context.Background(), plan))
		assert.Equal(t, []string{"plant/a", "plant/b"}, subscribedSince(conn, subCalls))
	})

	t.Run("MQTT 3.1.1 Reload whose DISCONNECT fails", func(t *testing.T) {
		s, conn, _ := reloadable(t, ProtocolVersion311)
		conn.failNextDisconnect.Store(true)
		require.Error(t, s.Reload(context.Background()), "Reload stops at the failed DISCONNECT")
		assert.Empty(t, s.Health(context.Background()).ActiveTopics, "the failed Reload dropped the record")

		require.NoError(t, s.Start(context.Background()), "the supervisor starts the session again")
		subCalls := conn.subscribeCallCount()
		connectionUp(s, true)
		require.NoError(t, s.Reconcile(context.Background(), plan))
		assert.Equal(t, []string{"plant/a", "plant/b"}, subscribedSince(conn, subCalls))
	})
}

// disconnectGeneration ends this session's hold on the broker session: a
// failed exclusive reconcile releases the lease right after it, and another
// owner may change the subscriptions before this session connects again.
func TestDisconnectGeneration_DropsTheMQTT311Record(t *testing.T) {
	plan := resumePlan(resumeSub("plant/a", 1), resumeSub("plant/b", 0))
	s, conn := newResumeTestSession(t, ProtocolVersion311, connectivity.SessionExclusive)
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return conn, func() {}, nil
	}
	connectionUp(s, false)
	require.NoError(t, s.Reconcile(context.Background(), plan))

	s.disconnectGeneration(context.Background())
	assert.Empty(t, s.Health(context.Background()).ActiveTopics)

	require.NoError(t, s.Start(context.Background()))
	subCalls := conn.subscribeCallCount()
	connectionUp(s, true)
	require.NoError(t, s.Reconcile(context.Background(), plan))
	assert.Equal(t, []string{"plant/a", "plant/b"}, subscribedSince(conn, subCalls))
}

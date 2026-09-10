package paho

import (
	"context"
	"sync"
	"testing"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// ═══════════════════════════════════════════════════════════════════════════
// (HIGH): No-Local is an OPT-IN per-session control (SessionOptions.NoLocal
// / `no_local`, default OFF). When enabled it sets MQTT5 NoLocal on every
// ordinary subscription so a same-broker MQTT->MQTT bridge does not receive —
// and re-forward — its OWN publishes (the self-amplification loop of Scenario
// 01). It defaults off so a session receiving its own publishes (the least-
// surprising MQTT contract) keeps working. A shared subscription ($share) MUST
// NOT set NoLocal even when opted in: MQTT5 §3.8.3.1 makes it a Protocol Error
// the broker rejects with a DISCONNECT.
//
// Mutation killed:
//   - drop the `s.opts.NoLocal &&` guard  → the default-off case sees NoLocal
//     true on the ordinary subscription and fails.
//   - drop the `&& !isSharedSubscriptionFilter(topic)` clause → the opt-in case
//     sees NoLocal true on the $share subscription and fails.
//   - hardcode NoLocal false → the opt-in case's ordinary assertion fails.
// ═══════════════════════════════════════════════════════════════════════════

// captureSubConn is a fake pahoConnection that records the subscribeSpecs it is
// asked to subscribe and grants the requested QoS for each.
type captureSubConn struct {
	mu   sync.Mutex
	subs []subscribeSpec
}

func (c *captureSubConn) AwaitConnection(context.Context) error { return nil }
func (c *captureSubConn) Disconnect(context.Context) error      { return nil }

func (c *captureSubConn) Subscribe(_ context.Context, subs []subscribeSpec) ([]byte, error) {
	c.mu.Lock()
	c.subs = append(c.subs, subs...)
	c.mu.Unlock()
	reasons := make([]byte, len(subs))
	for i, s := range subs {
		reasons[i] = s.QoS
	}
	return reasons, nil
}

func (c *captureSubConn) Unsubscribe(context.Context, []string) ([]byte, error) { return nil, nil }
func (c *captureSubConn) PublishEnvelope(
	context.Context, *messaging.Envelope, string, SenderOptions, clock.Clock,
) (publishResult, error) {
	return publishResult{}, nil
}
func (c *captureSubConn) Underlying() *autopaho.ConnectionManager { return nil }

func (c *captureSubConn) specFor(topic string) (subscribeSpec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.subs {
		if s.Topic == topic {
			return s, true
		}
	}
	return subscribeSpec{}, false
}

var _ pahoConnection = (*captureSubConn)(nil)

func TestBug_Subscribe_NoLocal_OptInPerSession(t *testing.T) {
	// reconcileNoLocal drives a reconcile with one ordinary and one shared
	// filter through a capturing fake and returns the issued subscribeSpecs.
	reconcileNoLocal := func(t *testing.T, noLocal bool) *captureSubConn {
		t.Helper()
		// Exclusive mode so a $share subscription is a legitimate configuration
		// (a single leaseholder), not the scale-out collision.
		sess := NewSession(SessionOptions{
			BrokerURLs: []string{"tcp://192.0.2.1:1883"},
			ClientID:   "nolocal",
			NoLocal:    noLocal,
			Clock:      testClock(),
		}, connectivity.SessionExclusive, nil)
		defer sess.Router().shutdown()

		fake := &captureSubConn{}
		sess.mu.Lock()
		sess.cm = fake
		sess.mu.Unlock()

		require.NoError(t, sess.Reconcile(context.Background(), connectivity.SessionPlan{
			Subscriptions: []connectivity.SubscriptionPlan{
				{Topic: "sensors/+/temp", QoS: 1},
				{Topic: "$share/g/commands/#", QoS: 1},
			},
		}))
		return fake
	}

	t.Run("default off leaves NoLocal clear", func(t *testing.T) {
		fake := reconcileNoLocal(t, false)

		ordinary, ok := fake.specFor("sensors/+/temp")
		require.True(t, ok, "the ordinary subscription was issued")
		require.False(t, ordinary.NoLocal,
			"no_local defaults off — an ordinary subscription must NOT set NoLocal")

		shared, ok := fake.specFor("$share/g/commands/#")
		require.True(t, ok, "the shared subscription was issued")
		require.False(t, shared.NoLocal, "a shared subscription never sets NoLocal")
	})

	t.Run("opt-in sets NoLocal for ordinary but never for shared", func(t *testing.T) {
		fake := reconcileNoLocal(t, true)

		ordinary, ok := fake.specFor("sensors/+/temp")
		require.True(t, ok, "the ordinary subscription was issued")
		require.True(t, ordinary.NoLocal,
			"with no_local enabled an ordinary subscription must set NoLocal to break the self-delivery loop")

		shared, ok := fake.specFor("$share/g/commands/#")
		require.True(t, ok, "the shared subscription was issued")
		require.False(t, shared.NoLocal,
			"a shared subscription ($share) must NOT set NoLocal even when opted in — MQTT5 §3.8.3.1 Protocol Error")
	})
}

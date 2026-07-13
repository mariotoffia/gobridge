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
// A-3 (MEDIUM): a reconcile whose SUBACK belongs to a PRIOR connection must not
// write activeSubs after a reconnect has reset it — otherwise an ephemeral
// (clean_start) session silently loses subscriptions.
//
// The narrow race the connEpoch guard closes:
//  1. Connection #1 comes up; handleConnectionUp resets activeSubs and the
//     manager starts a reconcile that snapshots the empty set and issues a
//     network SUBSCRIBE.
//  2. In the sub-microsecond window AFTER that SUBACK succeeds on connection #1
//     but BEFORE the reconcile writes activeSubs, connection #1 drops and #2
//     comes up — handleConnectionUp resets activeSubs to empty again.
//  3. The stale reconcile then writes its subscriptions into the FRESH set.
//     The next connect-edge reconcile computes desired−active = ∅ and skips the
//     re-subscribe. On a clean_start session (broker discarded the session)
//     this is silent subscription loss — the broker has no subscriptions but
//     activeSubs claims it does.
//
// handleConnectionUp bumps s.connEpoch when it resets activeSubs; reconcile
// captures the epoch with its snapshot and re-checks it before each write-back.
// A bump mid-flight makes the guard skip the stale write, leaving activeSubs
// empty so the authoritative reconnect reconcile re-subscribes in full.
//
// Mutation killed: delete either the `s.connEpoch++` in handleConnectionUp or
// the `if s.connEpoch == startEpoch` guard around the subscribe write-back →
// activeSubs retains "A" after the mid-flight reconnect and the final assertion
// fails.
// ═══════════════════════════════════════════════════════════════════════════

// reconnectDuringSubscribeConn is a fake pahoConnection whose SUBACK succeeds
// but whose Subscribe first runs an injected hook — used to land a reconnect in
// the exact window between a successful SUBACK and the reconcile write-back.
type reconnectDuringSubscribeConn struct {
	mu     sync.Mutex
	onSub  func()
	subbed [][]subscribeSpec
}

func (c *reconnectDuringSubscribeConn) AwaitConnection(context.Context) error { return nil }
func (c *reconnectDuringSubscribeConn) Disconnect(context.Context) error      { return nil }

func (c *reconnectDuringSubscribeConn) Subscribe(_ context.Context, subs []subscribeSpec) ([]byte, error) {
	c.mu.Lock()
	c.subbed = append(c.subbed, subs)
	hook := c.onSub
	c.mu.Unlock()
	if hook != nil {
		// Simulate a reconnect landing DURING the SUBACK round-trip.
		hook()
	}
	// SUBACK grants the requested QoS for every topic (all accepted).
	reasons := make([]byte, len(subs))
	for i, s := range subs {
		reasons[i] = s.QoS
	}
	return reasons, nil
}

func (c *reconnectDuringSubscribeConn) Unsubscribe(context.Context, []string) error { return nil }

func (c *reconnectDuringSubscribeConn) PublishEnvelope(
	context.Context, *messaging.Envelope, string, SenderOptions, clock.Clock,
) (publishResult, error) {
	return publishResult{}, nil
}
func (c *reconnectDuringSubscribeConn) Underlying() *autopaho.ConnectionManager { return nil }

func (c *reconnectDuringSubscribeConn) subscribeCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.subbed)
}

var _ pahoConnection = (*reconnectDuringSubscribeConn)(nil)

func TestBug_Reconcile_ReconnectMidFlight_SkipsStaleActiveSubsWriteBack(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "epoch-guard",
		UnmatchedGrace: testGrace,
		Clock:          testClock(),
	}, connectivity.SessionEphemeral, nil)
	defer sess.Router().shutdown()

	fake := &reconnectDuringSubscribeConn{}
	// The hook runs INSIDE cm.Subscribe, i.e. after reconcile snapshotted
	// activeSubs (and its epoch) but before the write-back: a faithful model of
	// connection #1 dropping and #2 coming up mid-round-trip. handleConnectionUp
	// is the REAL connect path — it resets activeSubs and bumps connEpoch.
	fake.onSub = func() { sess.handleConnectionUp() }

	sess.mu.Lock()
	sess.cm = fake
	// Model connection #1 already up (post-reset): epoch 1, empty activeSubs.
	sess.connEpoch = 1
	sess.activeSubs = map[string]byte{}
	sess.mu.Unlock()

	// Reconcile a plan wanting topic A. The SUBACK succeeds, but the injected
	// reconnect bumps the epoch mid-flight, so the write-back must be skipped.
	require.NoError(t, sess.Reconcile(context.Background(), connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "A", QoS: 1}},
	}))

	require.Equal(t, 1, fake.subscribeCalls(), "the reconcile issued exactly one SUBSCRIBE")

	sess.mu.Lock()
	_, hasA := sess.activeSubs["A"]
	epoch := sess.connEpoch
	sess.mu.Unlock()

	require.Greater(t, epoch, uint64(1), "the mid-flight reconnect bumped the connection epoch")
	require.False(t, hasA,
		"A-3: a SUBACK from a prior connection must NOT be written into the fresh activeSubs — "+
			"the reconnect reconcile must see an empty set and re-subscribe in full, not skip an empty delta")
}

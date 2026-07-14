package paho

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// reconcileProbeCM is a pahoConnection test double that records whether the
// context handed to each broker SUBSCRIBE / UNSUBSCRIBE carries a deadline and
// can optionally block until that context is cancelled (simulating a broker
// that accepts the connection but never returns SUBACK / UNSUBACK). It backs
// the HIGH-2 regression tests for the adapter-owned reconcile timeout.
type reconcileProbeCM struct {
	// block, when true, makes Subscribe/Unsubscribe wait for ctx.Done and
	// return ctx.Err() — a wedged broker op that must not hang the reconcile.
	block bool

	mu                sync.Mutex
	subscribeCalled   bool
	subscribeHadDDL   bool
	unsubscribeCalled bool
	unsubscribeHadDDL bool
}

func (c *reconcileProbeCM) AwaitConnection(ctx context.Context) error { return nil }
func (c *reconcileProbeCM) Disconnect(ctx context.Context) error      { return nil }
func (c *reconcileProbeCM) Underlying() *autopaho.ConnectionManager   { return nil }

func (c *reconcileProbeCM) PublishEnvelope(
	ctx context.Context,
	env *messaging.Envelope,
	topic string,
	opts SenderOptions,
	clk clock.Clock,
) (publishResult, error) {
	return publishResult{}, nil
}

func (c *reconcileProbeCM) Subscribe(ctx context.Context, subs []subscribeSpec) ([]byte, error) {
	_, hasDDL := ctx.Deadline()
	c.mu.Lock()
	c.subscribeCalled = true
	c.subscribeHadDDL = hasDDL
	c.mu.Unlock()
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	// One success reason (granted == requested QoS) per requested topic so
	// classifySubackReasons treats every subscription as accepted.
	reasons := make([]byte, len(subs))
	for i, s := range subs {
		reasons[i] = s.QoS
	}
	return reasons, nil
}

func (c *reconcileProbeCM) Unsubscribe(ctx context.Context, topics []string) ([]byte, error) {
	_, hasDDL := ctx.Deadline()
	c.mu.Lock()
	c.unsubscribeCalled = true
	c.unsubscribeHadDDL = hasDDL
	c.mu.Unlock()
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return make([]byte, len(topics)), nil
}

var _ pahoConnection = (*reconcileProbeCM)(nil)

// TestBug_Reconcile_BrokerOps_CarryAdapterOwnedDeadline proves HIGH-2: even
// when the reconcile ctx carries NO deadline (the runtime frequently passes a
// deadline-less context), the adapter wraps EACH broker SUBSCRIBE and
// UNSUBSCRIBE in its own context.WithTimeout(reconcile_timeout). Before the
// fix both ops received the bare caller ctx (no deadline) and the assertions
// below fail.
func TestBug_Reconcile_BrokerOps_CarryAdapterOwnedDeadline(t *testing.T) {
	s := NewSession(SessionOptions{
		ClientID:         "reconcile-deadline",
		ReconcileTimeout: DefaultReconcileTimeout,
		Clock:            testClock(),
	}, connectivity.SessionPersistent, nil)

	probe := &reconcileProbeCM{}

	// A brand-new subscription forces a SUBSCRIBE; a prior topic absent from
	// the desired plan forces an UNSUBSCRIBE — exercising BOTH wrapped ops.
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "live/route", QoS: 1}},
	}

	// context.Background() has NO deadline: the bound must come from the adapter.
	require.Nil(t, deadlineOf(context.Background()), "test precondition: caller ctx must be deadline-less")

	err := s.reconcile(context.Background(), probe, plan, []string{"stale/route"}, s.connEpoch)
	require.NoError(t, err)

	probe.mu.Lock()
	defer probe.mu.Unlock()
	require.True(t, probe.subscribeCalled, "reconcile must issue the SUBSCRIBE")
	require.True(t, probe.unsubscribeCalled, "reconcile must issue the UNSUBSCRIBE")
	require.True(t, probe.subscribeHadDDL,
		"HIGH-2: SUBSCRIBE ctx must carry the adapter-owned reconcile deadline")
	require.True(t, probe.unsubscribeHadDDL,
		"HIGH-2: UNSUBSCRIBE ctx must carry the adapter-owned reconcile deadline")
}

// TestBug_Reconcile_WedgedSubscribe_FailsBoundedNotHang proves HIGH-2's
// liveness guarantee: a SUBSCRIBE whose SUBACK never arrives (wedged broker)
// must fail via the adapter-owned timeout rather than hang the reconcile — and
// any startup / hot-reload step awaiting it — forever. The caller ctx is
// deadline-less, so only the adapter's timeout can unblock it.
func TestBug_Reconcile_WedgedSubscribe_FailsBoundedNotHang(t *testing.T) {
	s := NewSession(SessionOptions{
		ClientID:         "reconcile-wedge-sub",
		ReconcileTimeout: 30 * time.Millisecond,
		Clock:            testClock(),
	}, connectivity.SessionPersistent, nil)

	probe := &reconcileProbeCM{block: true}
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "live/route", QoS: 1}},
	}

	done := make(chan error, 1)
	go func() {
		done <- s.reconcile(context.Background(), probe, plan, nil, s.connEpoch)
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a wedged SUBSCRIBE must fail via the reconcile timeout, not hang")
		require.True(t, errors.Is(err, context.DeadlineExceeded),
			"HIGH-2: wedged SUBSCRIBE must surface as a deadline-exceeded timeout, got %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("HIGH-2 regression: reconcile hung on a wedged SUBSCRIBE past the adapter-owned timeout")
	}
}

// TestBug_Reconcile_WedgedUnsubscribe_FailsBoundedNotHang is the UNSUBSCRIBE
// twin of the SUBSCRIBE liveness test: a wedged UNSUBACK must not hang the
// reconcile either.
func TestBug_Reconcile_WedgedUnsubscribe_FailsBoundedNotHang(t *testing.T) {
	s := NewSession(SessionOptions{
		ClientID:         "reconcile-wedge-unsub",
		ReconcileTimeout: 30 * time.Millisecond,
		Clock:            testClock(),
	}, connectivity.SessionPersistent, nil)

	probe := &reconcileProbeCM{block: true}
	// Empty desired plan + a prior topic ⇒ an UNSUBSCRIBE with no SUBSCRIBE.
	done := make(chan error, 1)
	go func() {
		done <- s.reconcile(context.Background(), probe, connectivity.SessionPlan{}, []string{"stale/route"}, s.connEpoch)
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a wedged UNSUBSCRIBE must fail via the reconcile timeout, not hang")
		require.True(t, errors.Is(err, context.DeadlineExceeded),
			"HIGH-2: wedged UNSUBSCRIBE must surface as a deadline-exceeded timeout, got %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("HIGH-2 regression: reconcile hung on a wedged UNSUBSCRIBE past the adapter-owned timeout")
	}
}

func deadlineOf(ctx context.Context) *time.Time {
	if d, ok := ctx.Deadline(); ok {
		return &d
	}
	return nil
}

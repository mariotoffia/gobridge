package amqp10

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// failingCloseLink is a link whose closing detach the broker never confirms.
type failingCloseLink struct{ recordingLink }

func (l *failingCloseLink) Close(ctx context.Context) error {
	_ = l.recordingLink.Close(ctx)
	return errors.New("detach not acknowledged")
}

// attachedReceiver returns a receiver built from cfg with link attached over a
// connected session, and that connection.
func attachedReceiver(t *testing.T, cfg ReceiverConfig, link linkReceiver) (*Receiver, *mockConn) {
	t.Helper()
	sess := newTestSession()
	conn := &mockConn{}
	sess.mu.Lock()
	sess.conn = conn
	sess.connected = true
	sess.mu.Unlock()
	r, err := NewReceiver(cfg, sess)
	require.NoError(t, err)
	r.mu.Lock()
	r.link = link
	r.linkConn = conn
	r.mu.Unlock()
	return r, conn
}

func durableTopicReceiverConfig(metrics ports.MetricsExporter) ReceiverConfig {
	return ReceiverConfig{
		Address: "orders", DurabilityMode: 2, Routing: RoutingMulticast,
		SubscriptionName: "orders-sub", SessionID: "orders-session", Metrics: metrics,
	}
}

func linkCloses(link *recordingLink) (int, bool) {
	link.mu.Lock()
	defer link.mu.Unlock()
	return link.closeCalls, link.closeHadDDL
}

func TestReceiver_CloseEndsADurableTopicSubscriptionWhenAsked(t *testing.T) {
	link := &recordingLink{}
	r, conn := attachedReceiver(t, durableTopicReceiverConfig(nil), link)
	r.EndBrokerStateOnClose()

	require.NoError(t, r.Close(t.Context()))

	calls, bounded := linkCloses(link)
	assert.Equal(t, 1, calls, "a closing detach deletes the subscription")
	assert.True(t, bounded, "the detach is bounded")
	assert.False(t, connClosed(conn), "ending the subscription needs no connection drop")
}

func TestReceiver_CloseKeepsADurableTopicSubscriptionWithoutTheAsk(t *testing.T) {
	link := &recordingLink{}
	r, conn := attachedReceiver(t, durableTopicReceiverConfig(nil), link)

	require.NoError(t, r.Close(t.Context()))

	calls, _ := linkCloses(link)
	assert.Zero(t, calls, "a shutdown, pause or rebuild keeps the subscription")
	assert.True(t, connClosed(conn), "the subscription is kept by dropping the connection")
}

func TestReceiver_CloseNeverEndsAQueueReceiver(t *testing.T) {
	link := &recordingLink{}
	cfg := durableTopicReceiverConfig(nil)
	cfg.Routing = RoutingAnycast
	r, conn := attachedReceiver(t, cfg, link)
	r.EndBrokerStateOnClose()

	require.NoError(t, r.Close(t.Context()))

	calls, _ := linkCloses(link)
	assert.Zero(t, calls, "a queue receiver holds no subscription of its own")
	assert.True(t, connClosed(conn))
}

// TestReceiver_CloseWhileRunIsActiveKeepsTheSubscription pins the order drain,
// then end. The route runner force-closes a receiver whose Run is still active
// after ReceiverCloseTimeout, with deliveries possibly in flight. That close
// drops the connection as every other close does: it ends nothing and counts
// no failure, even when the receiver was asked.
//
// Mutation check: drop the Run-active check from closeLink and this close
// sends the closing detach instead of dropping the connection.
func TestReceiver_CloseWhileRunIsActiveKeepsTheSubscription(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	link := &recordingLink{}
	r, conn := attachedReceiver(t, durableTopicReceiverConfig(metrics), link)
	r.EndBrokerStateOnClose()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()
	t.Cleanup(func() {
		cancel()
		assert.ErrorIs(t, wait.RequireReceive(t, done, 2*time.Second), context.Canceled)
	})
	wait.Until(t, 2*time.Second, "Run to register the receiver on its session", func() bool {
		return r.session.runsReceiver(r)
	})

	require.NoError(t, r.Close(t.Context()))

	calls, _ := linkCloses(link)
	assert.Zero(t, calls, "a close while Run is active never sends the closing detach")
	assert.True(t, connClosed(conn), "the link is taken down by dropping the connection")
	assert.Empty(t, metrics.FindEntries(shared.MetricBrokerStateEndFailures),
		"a close that does not try to end the subscription counts no failure")
}

func TestReceiver_FallsBackToTheConnectionDropWhenTheSubscriptionCannotBeEnded(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	link := &failingCloseLink{}
	r, conn := attachedReceiver(t, durableTopicReceiverConfig(metrics), link)
	r.EndBrokerStateOnClose()

	require.NoError(t, r.Close(t.Context()), "a failure to end broker state never fails the reload")

	assert.True(t, connClosed(conn), "the link is still taken down")
	entries := metrics.FindEntries(shared.MetricBrokerStateEndFailures)
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "orders-session"})
}

func TestReceiver_TheAskLastsForOneClose(t *testing.T) {
	r, _ := attachedReceiver(t, durableTopicReceiverConfig(nil), &recordingLink{})
	r.EndBrokerStateOnClose()
	require.NoError(t, r.Close(t.Context()))

	second := &recordingLink{}
	r.mu.Lock()
	r.link = second
	r.mu.Unlock()
	require.NoError(t, r.Close(t.Context()))

	calls, _ := linkCloses(second)
	assert.Zero(t, calls)
}

func TestFactoryNewReceiver_KnowsItsSessionID(t *testing.T) {
	sess := newTestSession()
	raw, err := NewFactory(nil).NewReceiver(t.Context(), ports.ReceiverSpec{
		ID: "rx", SessionID: "orders-session", Config: &Config{Receiver: ReceiverParams{Address: "orders"}},
	}, sess)
	require.NoError(t, err)
	r, ok := raw.(*Receiver)
	require.True(t, ok)

	assert.Equal(t, "orders-session", r.cfg.SessionID)
}

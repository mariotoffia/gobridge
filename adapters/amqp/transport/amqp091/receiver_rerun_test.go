// Pins the ports.Receiver rule that Close ends one Run, not the receiver:
// the route runner calls Close when a Run ends and, on a supervised
// restart, calls Run again on the SAME receiver, which must open a fresh
// channel instead of failing or reusing the closed one.
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestReceiver_RunAfterCloseOpensFreshChannel runs a managed receiver on
// channel A, closes it, and runs it again. Close must close A, and Run #2
// must open a fresh channel B and emit a delivery from it.
//
// amqpChannel wraps the SDK's concrete *amqp.Channel, so conn.Channel()
// cannot hand Run a working channel in a unit test. Run #2 is therefore
// driven in two halves: Run itself, up to the point it asks the connection
// for a fresh channel, and then the consume attempt Run makes on that
// channel (runChannel) with a fake channel B.
//
// Mutation check: latch a closed flag in Close and return it from Run —
// Run #2 then returns before it opens a channel and the test fails.
func TestReceiver_RunAfterCloseOpensFreshChannel(t *testing.T) {
	opened := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseOpen := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOpen)
	mc := newMockConnection()
	mc.ChannelFn = func() (*amqpChannel, error) {
		opened <- struct{}{}
		<-release
		return nil, errors.New("channel open released by the test")
	}
	sess := newResilienceSession(nil)
	sess.conn = mc

	r := NewReceiver(ReceiverConfig{QueueName: "q", Session: sess, deferCloseToRunner: true})

	// Run #1 consumes on channel A; its graceful stop hands A to Close.
	chA := newFakeReceiverChannel()
	consumeOnce(t, r, chA, "a")
	require.NoError(t, r.Close(context.Background()))
	require.True(t, chA.isClosed(), "Close must close channel A")
	require.Equal(t, 1, chA.closeCount())

	// Run #2 must ask the connection for a fresh channel.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()
	select {
	case <-opened:
	case err := <-done:
		t.Fatalf("Run after Close returned %v before opening a fresh channel", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run after Close never asked the connection for a channel")
	}
	cancel()
	releaseOpen()
	require.ErrorIs(t, wait.RequireReceive(t, done, 2*time.Second), context.Canceled,
		"Run #2 must end on its own ctx, not on the prior Close")
	require.Equal(t, 1, mc.channelCalls(), "Run #2 must open exactly one fresh channel")

	// Run #2's consume attempt on the fresh channel B.
	chB := newFakeReceiverChannel()
	consumeOnce(t, r, chB, "b")
	require.NoError(t, r.Close(context.Background()))
	require.Equal(t, 1, chB.closeCount(), "Close after Run #2 must close channel B")
	require.Equal(t, 1, chA.closeCount(), "channel A must not be reused or closed again")
}

// consumeOnce drives one consume attempt on ch — the runChannel call Run
// makes with the channel it opened — until it emits wantID, stops it with a
// ctx cancel and settles the delivery, as the route runner does before
// Close.
func consumeOnce(t *testing.T, r *Receiver, ch *fakeReceiverChannel, wantID string) {
	t.Helper()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: wantID, Payload: []byte("x")})
	ch.deliveries <- NewDelivery(env,
		amqp.Delivery{Acknowledger: newMockAcknowledger(), DeliveryTag: 1, RoutingKey: "rk"},
		slog.Default(), &ports.NoopExporter{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got ports.Delivery
	err := r.runChannel(ctx, ch, func(_ context.Context, d ports.Delivery) error {
		got = d
		cancel()
		return nil
	})
	require.NoError(t, err, "a graceful stop must end the consume attempt cleanly")
	require.NotNil(t, got, "the consume attempt must emit from its channel")
	require.Equal(t, wantID, got.Envelope().ID())
	require.NoError(t, got.Ack(context.Background()))
}

// Pins the ports.Receiver rule that Close ends one Run, not the receiver,
// against a live RabbitMQ broker: the route runner closes a receiver when
// its Run ends and a supervised restart calls Run again on the SAME
// receiver, so the second Run must open a real channel again.
package amqp091

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// rerunDeliveryTimeout bounds one expected delivery.
const rerunDeliveryTimeout = 20 * time.Second

// rerunSilence is how long a Run must stay quiet to show an acknowledged
// message was not handed out again.
const rerunSilence = 2 * time.Second

// TestIntegration_ReceiverRunsAgainAfterClose runs one factory-built
// receiver (the managed mode a route uses: a graceful stop hands the channel
// to Close), closes it, and runs it again on the same instance:
//
//   - a message acknowledged in Run #1 is not delivered again, and a
//     message published between the runs reaches Run #2 on a fresh channel;
//   - a message still unacknowledged when Close runs is requeued by the
//     broker and delivered again in Run #2.
//
// Mutation check: latch a closed flag in Receiver.Close and return it from
// Run — Run #2 then returns before it opens a channel and both cases fail.
func TestIntegration_ReceiverRunsAgainAfterClose(t *testing.T) {
	t.Run("acked_message_is_not_redelivered", func(t *testing.T) {
		e := edge091Setup(t, slog.Default(), "rerun-acked")
		send := rerunSender(t, e)
		r := rerunReceiver(t, e)

		send("m1")
		run1 := startRerun(t, r)
		require.NoError(t, run1.next(t, "m1").Ack(t.Context()))
		chA := activeChannel(r)
		run1.stop(t)
		require.Same(t, chA, activeChannel(r), "a graceful stop must hand the channel to Close")
		require.NoError(t, r.Close(t.Context()))
		require.Nil(t, activeChannel(r), "Close must release the channel of Run #1")

		send("m2")
		run2 := startRerun(t, r)
		require.NoError(t, run2.next(t, "m2").Ack(t.Context()),
			"Run #2 must receive the message published between the runs")
		require.NotSame(t, chA, activeChannel(r), "Run #2 must consume on a fresh channel")
		wait.Silent(t, run2.got, rerunSilence)
		run2.stop(t)
		require.NoError(t, r.Close(t.Context()))
	})

	t.Run("unacked_message_is_requeued", func(t *testing.T) {
		e := edge091Setup(t, slog.Default(), "rerun-held")
		send := rerunSender(t, e)
		r := rerunReceiver(t, e)

		send("m3")
		run1 := startRerun(t, r)
		_ = run1.next(t, "m3") // held, never acknowledged: Close must requeue it
		run1.stop(t)
		require.NoError(t, r.Close(t.Context()))

		run2 := startRerun(t, r)
		require.NoError(t, run2.next(t, "m3").Ack(t.Context()),
			"a message held at Close must be delivered again, not lost")
		wait.Silent(t, run2.got, rerunSilence)
		run2.stop(t)
		require.NoError(t, r.Close(t.Context()))
	})
}

// rerunRun is one Run of the receiver under test.
type rerunRun struct {
	got    chan ports.Delivery
	done   chan error
	cancel context.CancelFunc
}

// startRerun starts r.Run in the background. Its emit hands each delivery
// to the test and gives up when the Run is cancelled, so an unread delivery
// can never keep the Run from stopping.
func startRerun(t *testing.T, r *Receiver) *rerunRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rr := &rerunRun{got: make(chan ports.Delivery, 16), done: make(chan error, 1), cancel: cancel}
	go func() {
		rr.done <- r.Run(ctx, func(ctx context.Context, d ports.Delivery) error {
			select {
			case rr.got <- d:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	t.Cleanup(cancel)
	return rr
}

// next waits for the Run's next delivery and requires it to be wantID. A
// Run that returns first fails the test: a Run ended by the prior Close is
// exactly the fault under test.
func (rr *rerunRun) next(t *testing.T, wantID string) ports.Delivery {
	t.Helper()
	select {
	case d := <-rr.got:
		require.Equal(t, wantID, d.Envelope().ID(), "unexpected delivery")
		return d
	case err := <-rr.done:
		t.Fatalf("Run returned %v before delivering %q", err, wantID)
	case <-time.After(rerunDeliveryTimeout):
		t.Fatalf("no delivery %q within %s", wantID, rerunDeliveryTimeout)
	}
	return nil
}

// stop cancels the Run, as the route runner does on a graceful stop, and
// requires it to end on that cancel.
func (rr *rerunRun) stop(t *testing.T) {
	t.Helper()
	rr.cancel()
	require.ErrorIs(t, wait.RequireReceive(t, rr.done, 10*time.Second), context.Canceled,
		"Run must end on its own ctx")
}

func activeChannel(r *Receiver) receiverChannel {
	r.chMu.Lock()
	defer r.chMu.Unlock()
	return r.activeCh
}

// rerunReceiver builds the receiver through Factory.NewReceiver, the path
// the bridge uses for every route.
func rerunReceiver(t *testing.T, e edge091Env) *Receiver {
	t.Helper()
	recv, err := NewFactory(slog.Default()).NewReceiver(t.Context(), ports.ReceiverSpec{
		ID:     "rerun",
		Config: &Config{Receiver: ReceiverParams{QueueName: e.queue}},
	}, e.sess)
	require.NoError(t, err)
	r, ok := recv.(*Receiver)
	require.True(t, ok, "factory receiver type = %T, want *Receiver", recv)
	require.True(t, r.cfg.deferCloseToRunner, "a factory receiver must hand its channel to Close")
	return r
}

// rerunSender returns a publish func for the queue of e.
func rerunSender(t *testing.T, e edge091Env) func(id string) {
	t.Helper()
	s := NewSender(SenderConfig{Exchange: e.exchange, RoutingKey: e.queue, Session: e.sess, Timeout: 10 * time.Second})
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return func(id string) {
		t.Helper()
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Subject: e.queue, Payload: []byte(id)})
		require.NoError(t, s.Send(t.Context(), ports.OutboundMessage{Envelope: env}), "publish %s", id)
	}
}

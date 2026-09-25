// Pins the ports.Receiver rule that Close ends one Run, not the receiver,
// against a live Artemis broker: the route runner closes a receiver when its
// Run ends and a supervised restart calls Run again on the SAME receiver, so
// the second Run must attach a real link again.
package amqp10

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// rerunDeliveryTimeout bounds one expected delivery. It covers a durable
// Close, which drops the connection, so Run #2 first waits for the session
// monitor to reconnect before it can attach.
const rerunDeliveryTimeout = 30 * time.Second

// rerunSilence is how long a Run must stay quiet to show an acknowledged
// message was not handed out again.
const rerunSilence = 2 * time.Second

// TestIntegration_ReceiverRunsAgainAfterClose runs one factory-built
// receiver, closes it, and runs it again on the same instance:
//
//   - a message acknowledged in Run #1 is not delivered again in Run #2,
//     and a message published after Run #2 attached reaches Run #2;
//   - a message still unsettled when Close runs is handed back to the
//     broker and delivered again in Run #2;
//   - a durable subscription keeps a message published between the runs:
//     its Close drops the connection instead of unsubscribing, and Run #2
//     re-attaches the same subscription once the session reconnects.
//
// Mutation check: latch a closed flag in Receiver.Close and return it from
// Run — Run #2 then returns before it attaches and every case fails.
func TestIntegration_ReceiverRunsAgainAfterClose(t *testing.T) {
	t.Run("acked_message_is_not_redelivered", func(t *testing.T) {
		sess := edgeSession(t, slog.Default())
		addr := artemislocal.UniqueAddress("rerun-acked")
		send := rerunSender(t, sess, addr, RoutingAnycast)
		r := rerunReceiver(t, sess, ReceiverParams{Address: addr, LinkCredit: 10})

		send("m1")
		run1 := startRerun(t, r)
		require.NoError(t, run1.next(t, "m1").Ack(t.Context()))
		run1.stop(t)
		require.NoError(t, r.Close(t.Context()))
		require.False(t, linkAttached(r), "Close must detach the link of Run #1")

		run2 := startRerun(t, r)
		// Publish only once Run #2 holds a link: Artemis may auto-delete the
		// emptied, consumer-less queue between the runs, and a message sent
		// while no queue is bound would be dropped by the broker, not the
		// receiver.
		run2.waitAttached(t, r)
		send("m2")
		require.NoError(t, run2.next(t, "m2").Ack(t.Context()), "Run #2 must receive a new message")
		wait.Silent(t, run2.got, rerunSilence)
		run2.stop(t)
		require.NoError(t, r.Close(t.Context()))
	})

	t.Run("unsettled_message_is_handed_back", func(t *testing.T) {
		sess := edgeSession(t, slog.Default())
		addr := artemislocal.UniqueAddress("rerun-held")
		send := rerunSender(t, sess, addr, RoutingAnycast)
		r := rerunReceiver(t, sess, ReceiverParams{Address: addr, LinkCredit: 10})

		send("m3")
		run1 := startRerun(t, r)
		_ = run1.next(t, "m3") // held, never settled: Close must hand it back
		run1.stop(t)
		// Close waits for in-flight settlements up to its ctx; m3 never
		// settles, so bound it the way the route runner does.
		closeBounded(t, r)
		require.False(t, linkAttached(r), "Close must detach the link even with a delivery in flight")

		run2 := startRerun(t, r)
		require.NoError(t, run2.next(t, "m3").Ack(t.Context()),
			"a message held at Close must be delivered again, not lost")
		wait.Silent(t, run2.got, rerunSilence)
		run2.stop(t)
		// The m3 handle from Run #1 is never settled, so it still counts as
		// in flight and this Close waits out its bound too.
		closeBounded(t, r)
	})

	t.Run("durable_subscription_keeps_messages_between_runs", func(t *testing.T) {
		addr := artemislocal.UniqueAddress("rerun-durable")
		// The durable receiver gets a dedicated session with an explicit
		// container_id (Factory.NewReceiver enforces both); the publisher
		// lives on its own session so the durable Close cannot blip it.
		sess := durableRerunSession(t, "gobridge-rerun-"+addr)
		send := rerunSender(t, edgeSession(t, slog.Default()), addr, RoutingMulticast)
		r := rerunReceiver(t, sess, ReceiverParams{
			Address:          addr,
			LinkCredit:       10,
			DurabilityMode:   2,
			Routing:          RoutingMulticast,
			SubscriptionName: "rerun-durable",
		})

		run1 := startRerun(t, r)
		// A multicast message reaches only subscriptions that exist, so wait
		// for the durable link to attach before publishing.
		wait.RequireClosed(t, r.Started(), rerunDeliveryTimeout)
		send("d1")
		require.NoError(t, run1.next(t, "d1").Ack(t.Context()))
		run1.stop(t)
		require.NoError(t, r.Close(t.Context()))

		send("d2") // no link attached: only the durable subscription holds it
		run2 := startRerun(t, r)
		require.NoError(t, run2.next(t, "d2").Ack(t.Context()),
			"Run #2 must re-attach the durable subscription and receive what it kept")
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

// waitAttached waits until the Run holds a live link, failing at once if
// the Run returns first.
func (rr *rerunRun) waitAttached(t *testing.T, r *Receiver) {
	t.Helper()
	wait.Until(t, rerunDeliveryTimeout, "Run to attach a link", func() bool {
		select {
		case err := <-rr.done:
			t.Fatalf("Run returned %v before it attached a link", err)
		default:
		}
		return linkAttached(r)
	})
}

// stop cancels the Run, as the route runner does on a graceful stop, and
// requires it to end on that cancel.
func (rr *rerunRun) stop(t *testing.T) {
	t.Helper()
	rr.cancel()
	require.ErrorIs(t, wait.RequireReceive(t, rr.done, 10*time.Second), context.Canceled,
		"Run must end on its own ctx")
}

func linkAttached(r *Receiver) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.link != nil
}

func closeBounded(t *testing.T, r *Receiver) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, r.Close(ctx))
}

// rerunReceiver builds the receiver through Factory.NewReceiver, the path
// the bridge uses for every route.
func rerunReceiver(t *testing.T, sess *Session, params ReceiverParams) *Receiver {
	t.Helper()
	recv, err := NewFactory(slog.Default()).NewReceiver(t.Context(), ports.ReceiverSpec{
		ID:     "rerun",
		Config: &Config{Receiver: params},
	}, sess)
	require.NoError(t, err)
	r, ok := recv.(*Receiver)
	require.True(t, ok, "factory receiver type = %T, want *Receiver", recv)
	return r
}

// rerunSender returns a publish func for addr on sess.
func rerunSender(t *testing.T, sess *Session, addr string, routing RoutingType) func(id string) {
	t.Helper()
	s, err := NewSender(SenderConfig{Address: addr, Session: sess, Routing: routing, Timeout: 10 * time.Second}, sess)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return func(id string) {
		t.Helper()
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Subject: "rerun", Payload: []byte(id)})
		require.NoError(t, s.Send(t.Context(), ports.OutboundMessage{Envelope: env}), "publish %s", id)
	}
}

// durableRerunSession starts a session with an explicit container_id, which
// a durable subscription's identity needs.
func durableRerunSession(t *testing.T, containerID string) *Session {
	t.Helper()
	user, pass := artemislocal.Credentials()
	sess := NewSession(SessionOptions{
		Address:        artemislocal.Endpoint(t),
		Username:       user,
		Password:       shared.NewSecret(pass),
		ConnectTimeout: 15 * time.Second,
		IdleTimeout:    time.Minute,
		ContainerID:    containerID,
	}, connectivity.SessionEphemeral, slog.Default())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	require.NoError(t, sess.Start(ctx))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}
